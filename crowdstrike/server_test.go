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

const aid = "123456789abcdef0123456789abcdef0"

// The mock follows the shapes of the CrowdStrike open source clients
// (github.com/CrowdStrike/falconpy, github.com/CrowdStrike/gofalcon):
// POST /oauth2/token answers 201 with an access token, and
// POST /devices/entities/devices-actions/v2?action_name=... answers 202 with
// {"meta","errors","resources":[{"id","path"}]}. An unknown host gets an
// entry in "errors", and a bad token gets 401.
type mock struct {
	calls     []string
	bodies    []string
	tokenForm map[string]string
	contained bool
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/oauth2/token" {
		_ = r.ParseForm()
		m.tokenForm = map[string]string{}
		for k := range r.PostForm {
			m.tokenForm[k] = r.PostForm.Get(k)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"access_token":"falcon-token","expires_in":1799,"token_type":"bearer"}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer falcon-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":401,"message":"access denied, invalid bearer token"}]}`))
		return
	}
	b, _ := io.ReadAll(r.Body)
	m.calls = append(m.calls, r.Method+" "+r.URL.RequestURI())
	m.bodies = append(m.bodies, string(b))
	var body struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(b, &body)
	if r.URL.Path != "/devices/entities/devices-actions/v2" || len(body.IDs) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"code":400,"message":"bad request"}]}`))
		return
	}
	if body.IDs[0] != aid {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"meta":{"query_time":0.01,"trace_id":"t"},"errors":[{"code":404,"message":"Device not found"}],"resources":[]}`))
		return
	}
	m.contained = r.URL.Query().Get("action_name") == "contain"
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"meta":{"query_time":0.01,"trace_id":"t"},"errors":[],"resources":[{"id":"` + aid + `","path":"/devices/entities/devices/v2?ids=` + aid + `"}]}`))
}

func setup(t *testing.T) (*mock, func(string, map[string]any) (bool, string), map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"FALCON_BASE_URL": ts.URL, "FALCON_CLIENT_ID": "cid", "FALCON_CLIENT_SECRET": "csecret",
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

func TestContainAndLift(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("contain_host", map[string]any{"host_id": aid})
	if isErr || !strings.Contains(text, `"accepted":true`) || !m.contained {
		t.Fatalf("contain: %v %s", isErr, text)
	}
	if m.calls[0] != "POST /devices/entities/devices-actions/v2?action_name=contain" || m.bodies[0] != `{"ids":["`+aid+`"]}` {
		t.Errorf("calls=%v bodies=%v", m.calls, m.bodies)
	}
	if m.tokenForm["client_id"] != "cid" || m.tokenForm["client_secret"] != "csecret" || m.tokenForm["grant_type"] != "" {
		t.Errorf("token form = %v", m.tokenForm)
	}
	isErr, text = call("lift_containment", map[string]any{"host_id": aid})
	if isErr || m.contained || m.calls[1] != "POST /devices/entities/devices-actions/v2?action_name=lift_containment" {
		t.Fatalf("lift: %v %s %v", isErr, text, m.calls)
	}
}

func TestUnknownHostIsAnError(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text := call("contain_host", map[string]any{"host_id": strings.Repeat("a", 32)})
	if !isErr || !strings.Contains(text, "Device not found") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "host-01", aid + "0", aid[:31], aid[:31] + "g", "*", aid + ",abc", strings.Repeat("a", 500)} {
		for _, tool := range []string{"contain_host", "lift_containment"} {
			if isErr, _ := call(tool, map[string]any{"host_id": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
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
	kittest.AssertReversible(t, tools, "contain_host", "lift_containment")
	kittest.AssertReversible(t, tools, "lift_containment", "contain_host")
	kittest.AssertIdempotent(t, tools, "contain_host", true)
	kittest.AssertIdempotent(t, tools, "lift_containment", true)
}

func TestConfigErrors(t *testing.T) {
	if _, err := build(kit.NewEnvFrom(nil), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("empty environment accepted")
	}
}
