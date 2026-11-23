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

const uid = "87d349ed-44d7-43e1-9a83-5f2406dee5bd"

// The mock follows Microsoft Graph v1.0:
// https://learn.microsoft.com/en-us/graph/api/user-revokesigninsessions
// (POST, 200, {"@odata.context":..., "value": true}) and
// https://learn.microsoft.com/en-us/graph/api/user-update (PATCH, 204).
// The token endpoint is the client credentials flow of the Microsoft identity
// platform. The error body is {"error":{"code","message"}}.
type mock struct {
	calls      []string
	bodies     []string
	auth       []string
	tokenForm  map[string]string
	tokenCalls int
	enabled    bool
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/contoso.onmicrosoft.com/oauth2/v2.0/token" {
		m.tokenCalls++
		_ = r.ParseForm()
		m.tokenForm = map[string]string{}
		for k := range r.PostForm {
			m.tokenForm[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3599,"access_token":"graph-token"}`))
		return
	}
	b, _ := io.ReadAll(r.Body)
	m.calls = append(m.calls, r.Method+" "+r.URL.EscapedPath())
	m.bodies = append(m.bodies, string(b))
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	switch {
	case !strings.Contains(r.URL.Path, uid):
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"Request_ResourceNotFound","message":"Resource 'x' does not exist."}}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1.0/users/"+uid+"/revokeSignInSessions":
		_, _ = w.Write([]byte(`{"@odata.context":"https://graph.microsoft.com/v1.0/$metadata#Edm.Boolean","value":true}`))
	case r.Method == http.MethodPatch && r.URL.Path == "/v1.0/users/"+uid:
		var body map[string]bool
		_ = json.Unmarshal(b, &body)
		m.enabled = body["accountEnabled"]
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (bool, string), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{enabled: true}
	ts := httptest.NewServer(wrap(m))
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"ENTRA_TENANT_ID": "contoso.onmicrosoft.com", "ENTRA_CLIENT_ID": "app-id", "ENTRA_CLIENT_SECRET": "s3cret",
		"ENTRA_GRAPH_URL": ts.URL, "ENTRA_AUTHORITY_URL": ts.URL,
	}), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}, kittest.Tools(t, cs)
}

func TestRevokeSignInSessions(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("revoke_sign_in_sessions", map[string]any{"user_id": uid})
	if isErr || !strings.Contains(text, `"done":true`) {
		t.Fatalf("isErr=%v %s", isErr, text)
	}
	if m.calls[0] != "POST /v1.0/users/"+uid+"/revokeSignInSessions" || m.auth[0] != "Bearer graph-token" {
		t.Errorf("calls=%v auth=%v", m.calls, m.auth)
	}
	f := m.tokenForm
	if f["grant_type"] != "client_credentials" || f["client_id"] != "app-id" || f["client_secret"] != "s3cret" ||
		f["scope"] != "https://graph.microsoft.com/.default" {
		t.Errorf("token form = %v", f)
	}
	call("revoke_sign_in_sessions", map[string]any{"user_id": uid})
	if m.tokenCalls != 1 {
		t.Errorf("token calls = %d, want 1 (cached)", m.tokenCalls)
	}
}

func TestDisableAndEnable(t *testing.T) {
	m, call, _ := setup(t)
	if isErr, text := call("disable_account", map[string]any{"user_id": uid}); isErr || m.enabled {
		t.Fatalf("disable: %v %s", isErr, text)
	}
	if m.calls[0] != "PATCH /v1.0/users/"+uid || m.bodies[0] != `{"accountEnabled":false}` {
		t.Errorf("calls=%v bodies=%v", m.calls, m.bodies)
	}
	if isErr, text := call("enable_account", map[string]any{"user_id": uid}); isErr || !m.enabled {
		t.Fatalf("enable: %v %s", isErr, text)
	}
	if m.bodies[1] != `{"accountEnabled":true}` {
		t.Errorf("bodies=%v", m.bodies)
	}
}

func TestUnknownUserShowsGraphError(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text := call("disable_account", map[string]any{"user_id": "00000000-0000-0000-0000-000000000000"})
	if !isErr || !strings.Contains(text, "Request_ResourceNotFound") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "alice@contoso.com", uid + "/memberOf", "../me", uid[:35], strings.ToUpper(uid) + "x", "me"} {
		for _, tool := range []string{"revoke_sign_in_sessions", "disable_account", "enable_account"} {
			if isErr, _ := call(tool, map[string]any{"user_id": bad}); !isErr {
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
	if len(tools) != 3 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertIrreversible(t, tools, "revoke_sign_in_sessions")
	kittest.AssertReversible(t, tools, "disable_account", "enable_account")
	kittest.AssertReversible(t, tools, "enable_account", "disable_account")
	for name := range tools {
		kittest.AssertIdempotent(t, tools, name, true)
	}
}

func TestConfigErrors(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := build(kit.NewEnvFrom(nil), log); err == nil {
		t.Error("empty environment accepted")
	}
	_, err := build(kit.NewEnvFrom(map[string]string{"ENTRA_TENANT_ID": "a/../b", "ENTRA_CLIENT_ID": "x", "ENTRA_CLIENT_SECRET": "y"}), log)
	if err == nil {
		t.Error("bad tenant accepted")
	}
}
