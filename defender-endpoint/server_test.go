package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

const dev = "1e5bc9d7e413ddd7902c2932e418702b84d0cc07"

// The mock follows the Defender for Endpoint API:
// https://learn.microsoft.com/en-us/defender-endpoint/api/isolate-machine
// https://learn.microsoft.com/en-us/defender-endpoint/api/unisolate-machine
// Both answer 201 with a machineAction (https://learn.microsoft.com/en-us/defender-endpoint/api/machineaction).
// A second unisolate while one is pending gives HTTP 400, "Action is already in progress".
type mock struct {
	calls     []string
	bodies    []string
	auth      []string
	tokenForm map[string]string
	pending   bool
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
		_ = r.ParseForm()
		m.tokenForm = map[string]string{}
		for k := range r.PostForm {
			m.tokenForm[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3599,"access_token":"mde-token"}`))
		return
	}
	b, _ := io.ReadAll(r.Body)
	m.calls = append(m.calls, r.Method+" "+r.URL.EscapedPath())
	m.bodies = append(m.bodies, string(b))
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	typ := ""
	switch r.URL.Path {
	case "/api/machines/" + dev + "/isolate":
		typ = "Isolate"
	case "/api/machines/" + dev + "/unisolate":
		typ = "Unisolate"
		if m.pending {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"BadRequest","message":"Action is already in progress"}}`))
			return
		}
		m.pending = true
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ResourceNotFound","message":"Machine not found"}}`))
		return
	}
	var req map[string]string
	_ = json.Unmarshal(b, &req)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":"5382f7ea-7557-4ab7-9782-d50480024a4e","type":"` + typ + `","scope":"` + req["IsolationType"] +
		`","requestor":"app","requestorComment":"` + req["Comment"] + `","status":"Pending","machineId":"` + dev +
		`","computerDnsName":"desktop-test","creationDateTimeUtc":"2026-11-20T14:39:38Z","relatedFileInfo":null}`))
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (bool, string), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewServer(wrap(m))
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"DEFENDER_TENANT_ID": "tenant-guid", "DEFENDER_CLIENT_ID": "cid", "DEFENDER_CLIENT_SECRET": "csecret",
		"DEFENDER_API_URL": ts.URL, "DEFENDER_AUTHORITY_URL": ts.URL,
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}, kittest.Tools(t, cs)
}

func TestIsolate(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("isolate_device", map[string]any{"device_id": dev, "comment": "alert 1234"})
	if isErr || !strings.Contains(text, `"type":"Isolate"`) || !strings.Contains(text, `"status":"Pending"`) {
		t.Fatalf("isErr=%v %s", isErr, text)
	}
	if m.calls[0] != "POST /api/machines/"+dev+"/isolate" || m.auth[0] != "Bearer mde-token" {
		t.Errorf("calls=%v auth=%v", m.calls, m.auth)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(m.bodies[0]), &body)
	if body["IsolationType"] != "Full" || body["Comment"] != "Zero Root: alert 1234" {
		t.Errorf("body = %v", body)
	}
	if m.tokenForm["scope"] != "https://api.securitycenter.microsoft.com/.default" || m.tokenForm["grant_type"] != "client_credentials" {
		t.Errorf("token form = %v", m.tokenForm)
	}
}

func TestReleaseAndPendingRelease(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("release_device", map[string]any{"device_id": dev})
	if isErr || !strings.Contains(text, `"type":"Unisolate"`) {
		t.Fatalf("isErr=%v %s", isErr, text)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(m.bodies[0]), &body)
	if _, has := body["IsolationType"]; has || body["Comment"] != "Zero Root: response action" {
		t.Errorf("body = %v", body)
	}
	isErr, text = call("release_device", map[string]any{"device_id": dev})
	if !isErr || !strings.Contains(text, "already in progress") {
		t.Errorf("second release: %v %s", isErr, text)
	}
}

func TestUnknownDevice(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text := call("isolate_device", map[string]any{"device_id": strings.Repeat("a", 40)})
	if !isErr || !strings.Contains(text, "Machine not found") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "desktop-test", dev + "0", dev[:39], dev[:39] + "z", "../isolate", dev + "/isolate"} {
		for _, tool := range []string{"isolate_device", "release_device"} {
			if isErr, _ := call(tool, map[string]any{"device_id": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
	}
	for name, args := range map[string]map[string]any{
		"bad type":     {"device_id": dev, "isolation_type": "Everything"},
		"long comment": {"device_id": dev, "comment": strings.Repeat("c", 201)},
		"control char": {"device_id": dev, "comment": "a\nb"},
	} {
		if isErr, _ := call("isolate_device", args); !isErr {
			t.Errorf("%s accepted", name)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("an invalid input reached the vendor: %v", m.calls)
	}
}

func TestAnnotations(t *testing.T) {
	_, _, tools := setup(t)
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, "isolate_device", "release_device")
	kittest.AssertReversible(t, tools, "release_device", "isolate_device")
	kittest.AssertIdempotent(t, tools, "isolate_device", true)
	kittest.AssertIdempotent(t, tools, "release_device", true)
}
