package main

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the orchestrator API of Puppet Enterprise:
// POST /orchestrator/v1/command/task answers 202 {"job":{"id","name"}}
// (https://help.puppet.com/pe/current/topics/orchestrator_api_post_command_task.htm),
// GET /orchestrator/v1/jobs/{id} answers the job
// (https://help.puppet.com/pe/current/topics/orchestrator_api_get_jobs_job_id.htm), and
// GET /orchestrator/v1/jobs/{id}/nodes answers {"items":[...],"next-events":{...}}
// (https://help.puppet.com/pe/current/topics/orchestrator_api_get_jobs_job_id_nodes.htm).
// The token goes in the header X-Authentication.
type mock struct {
	calls  []string
	bodies []string
	token  string
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := io.ReadAll(r.Body)
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.bodies = append(m.bodies, string(b))
	m.token = r.Header.Get("X-Authentication")
	if m.token != "tok" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"kind":"puppetlabs.orchestrator/unauthorized","msg":"Authentication required"}`))
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/orchestrator/v1/command/task":
		var req struct {
			Scope struct {
				Nodes []string `json:"nodes"`
			} `json:"scope"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.Scope.Nodes) != 1 || req.Scope.Nodes[0] != "web01.example.com" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"kind":"puppetlabs.orchestrator/empty-target","msg":"Node not found"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"https://orchestrator.example.com:8143/orchestrator/v1/jobs/81","name":"81"}}`))
	case r.URL.Path == "/orchestrator/v1/jobs/81":
		_, _ = w.Write([]byte(`{"id":"https://orchestrator.example.com:8143/orchestrator/v1/jobs/81","type":"task","state":"finished",` +
			`"options":{"noop":false,"environment":"production","task":"pe_patch::patch_server","params":{"reboot":"never"}},` +
			`"node_count":1,"timestamp":"2026-11-20T16:45:31Z",` +
			`"status":[{"state":"new","enter_time":"2026-11-20T16:44:31Z"}],` +
			`"nodes":{"id":"https://orchestrator.example.com:8143/orchestrator/v1/jobs/81/nodes"}}`))
	case r.URL.Path == "/orchestrator/v1/jobs/82":
		// A job that the patch server did not start: another task.
		_, _ = w.Write([]byte(`{"id":"https://orchestrator.example.com:8143/orchestrator/v1/jobs/82","type":"task","state":"finished",` +
			`"options":{"environment":"production","task":"acme::read_secrets","params":{}},"node_count":40}`))
	case r.URL.Path == "/orchestrator/v1/jobs/83":
		// A Puppet run, not a task.
		_, _ = w.Write([]byte(`{"id":"https://orchestrator.example.com:8143/orchestrator/v1/jobs/83","type":"deploy","state":"finished",` +
			`"options":{"environment":"production","noop":false},"node_count":40}`))
	case r.URL.Path == "/orchestrator/v1/jobs/81/nodes", r.URL.Path == "/orchestrator/v1/jobs/82/nodes",
		r.URL.Path == "/orchestrator/v1/jobs/83/nodes":
		_, _ = w.Write([]byte(`{"next-events":{"id":"https://orchestrator.example.com:8143/orchestrator/v1/jobs/81/events?start=10"},` +
			`"items":[{"duration":48.0,"state":"finished","name":"web01.example.com","details":{"message":"patched 12 packages"},` +
			`"start_timestamp":"2026-11-20T16:44:40Z","finish_timestamp":"2026-11-20T16:45:28Z","result":{"output":"ok"}}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"puppetlabs.orchestrator/unknown-job","msg":"Unknown job"}`))
	}
}

func baseEnv(url string) map[string]string {
	return map[string]string{
		"PUPPET_ORCHESTRATOR_URL": url, "PUPPET_TOKEN": "tok",
		"PUPPET_PATCH_TASK": "pe_patch::patch_server", "PUPPET_PATCH_PARAMS": `{"reboot":"never"}`,
		"PUPPET_ROLLBACK_TASK": "acme::rollback_patch",
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (bool, string), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(baseEnv(ts.URL)), slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}, kittest.Tools(t, cs)
}

func TestStartPatchAndRollback(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("start_patch_job", map[string]any{"node": "web01.example.com"})
	if isErr || !strings.Contains(text, `"job_id":"81"`) || !strings.Contains(text, "pe_patch::patch_server") {
		t.Fatalf("patch: %v %s", isErr, text)
	}
	var body struct {
		Environment string
		Task        string
		Params      map[string]string
		Scope       map[string][]string
		Description string
	}
	_ = json.Unmarshal([]byte(m.bodies[0]), &body)
	if body.Environment != "production" || body.Task != "pe_patch::patch_server" || body.Params["reboot"] != "never" ||
		body.Scope["nodes"][0] != "web01.example.com" || !strings.HasPrefix(body.Description, "Zero Root") {
		t.Errorf("body = %+v", body)
	}
	if m.token != "tok" {
		t.Errorf("token = %q", m.token)
	}
	isErr, text = call("start_rollback_job", map[string]any{"node": "web01.example.com"})
	_ = json.Unmarshal([]byte(m.bodies[1]), &body)
	if isErr || body.Task != "acme::rollback_patch" || !strings.Contains(text, `"job_id":"81"`) {
		t.Errorf("rollback: %v %s %+v", isErr, text, body)
	}
}

func TestGetJob(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("get_job", map[string]any{"job_id": "81"})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	for _, want := range []string{`"state":"finished"`, `"task":"pe_patch::patch_server"`, `"node_count":1`, `"name":"web01.example.com"`, "patched 12 packages", `"output":"ok"`} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %s: %s", want, text)
		}
	}
	if m.calls[0] != "GET /orchestrator/v1/jobs/81" || m.calls[1] != "GET /orchestrator/v1/jobs/81/nodes" {
		t.Errorf("calls = %v", m.calls)
	}
}

// TestGetJobRefusesOtherJobs asserts that get_job shows only a job of the
// patch task or the rollback task of the operator. The model cannot read the
// nodes or the output of any other orchestrator job.
func TestGetJobRefusesOtherJobs(t *testing.T) {
	m, call, _ := setup(t)
	for _, id := range []string{"82", "83"} {
		m.calls = nil
		isErr, text := call("get_job", map[string]any{"job_id": id})
		if !isErr || !strings.Contains(text, "not a job of the patch task or the rollback task") {
			t.Errorf("job %s: isErr=%v %s", id, isErr, text)
		}
		if len(m.calls) != 1 || m.calls[0] != "GET /orchestrator/v1/jobs/"+id {
			t.Errorf("job %s: calls = %v, want only the job read", id, m.calls)
		}
	}
}

func TestVendorErrorsShown(t *testing.T) {
	_, call, _ := setup(t)
	if isErr, text := call("start_patch_job", map[string]any{"node": "db99.example.com"}); !isErr || !strings.Contains(text, "empty-target") {
		t.Errorf("unknown node: %v %s", isErr, text)
	}
	if isErr, text := call("get_job", map[string]any{"job_id": "5"}); !isErr || !strings.Contains(text, "unknown-job") {
		t.Errorf("unknown job: %v %s", isErr, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "Web01", "web01 example.com", "web01.example.com,db01", "*", "../x", "-web", "web01;ls", strings.Repeat("a", 300)} {
		for _, tool := range []string{"start_patch_job", "start_rollback_job"} {
			if isErr, _ := call(tool, map[string]any{"node": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
	}
	for _, bad := range []string{"", "81/nodes", "abc", "1234567890123", "-1", "81?x=1"} {
		if isErr, _ := call("get_job", map[string]any{"job_id": bad}); !isErr {
			t.Errorf("get_job accepted %q", bad)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("an invalid input reached the vendor: %v", m.calls)
	}
}

func TestAnnotations(t *testing.T) {
	_, _, tools := setup(t)
	if len(tools) != 3 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, "start_patch_job", "start_rollback_job")
	kittest.AssertReversible(t, tools, "start_rollback_job", "start_patch_job")
	kittest.AssertIdempotent(t, tools, "start_patch_job", false)
	if a := tools["get_job"].Annotations; !a.ReadOnlyHint {
		t.Error("get_job must be read only")
	}
}

func TestConfigErrors(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for name, mutate := range map[string]func(map[string]string){
		"no rollback task": func(m map[string]string) { delete(m, "PUPPET_ROLLBACK_TASK") },
		"bad task name":    func(m map[string]string) { m["PUPPET_PATCH_TASK"] = "pe_patch::patch_server; rm" },
		"bad params":       func(m map[string]string) { m["PUPPET_PATCH_PARAMS"] = `[1]` },
		"plain http":       func(m map[string]string) { m["PUPPET_ORCHESTRATOR_URL"] = "http://pe.example.com:8143" },
		"bad environment":  func(m map[string]string) { m["PUPPET_ENVIRONMENT"] = "../x" },
	} {
		cfg := baseEnv("https://pe.example.com:8143")
		mutate(cfg)
		if _, err := buildWith(kit.NewEnvFrom(cfg), log, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClientWithCA(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))
	defer ts.Close()
	dir := t.TempDir()
	good := filepath.Join(dir, "ca.pem")
	pemBytes := pemOf(ts)
	if err := os.WriteFile(good, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientWithCA(good); err != nil {
		t.Errorf("good CA: %v", err)
	}
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientWithCA(bad); err == nil {
		t.Error("bad CA accepted")
	}
	if _, err := clientWithCA(filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing file accepted")
	}
}

func pemOf(ts *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
}
