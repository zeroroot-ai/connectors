package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

const uid = "00ub0oNGTSWTBKOLGLNR"

// The mock follows the Okta Management API:
// https://developer.okta.com/docs/api/openapi/okta-management/management/
// and the OpenAPI spec https://github.com/okta/okta-management-openapi-spec.
// Revoke sessions answers 204. Suspend and unsuspend answer 200 with an empty
// body. A user in the wrong state gets 400 with the Okta error object. An
// unknown user gets 404 with the error code E0000007.
type mock struct {
	calls  []string
	auth   string
	status string // the status of the user: ACTIVE or SUSPENDED
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.calls = append(m.calls, r.Method+" "+r.URL.RequestURI())
	m.auth = r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	if !strings.Contains(r.URL.Path, uid) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorCode":"E0000007","errorSummary":"Not found: Resource not found: x (User)","errorLink":"E0000007","errorId":"e1","errorCauses":[]}`))
		return
	}
	bad := func() {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorCode":"E0000001","errorSummary":"Api validation failed: wrong state","errorLink":"E0000001","errorId":"e2","errorCauses":[]}`))
	}
	switch {
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/users/"+uid+"/sessions":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lifecycle/suspend"):
		if m.status != "ACTIVE" {
			bad()
			return
		}
		m.status = "SUSPENDED"
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lifecycle/unsuspend"):
		if m.status != "SUSPENDED" {
			bad()
			return
		}
		m.status = "ACTIVE"
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setup(t *testing.T) (*mock, func(string, map[string]any) (bool, string), map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{status: "ACTIVE"}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{"OKTA_ORG_URL": ts.URL, "OKTA_API_TOKEN": "tok"}),
		slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	call := func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}
	return m, call, kittest.Tools(t, cs)
}

func TestRevokeSessions(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("revoke_user_sessions", map[string]any{"user_id": uid})
	if isErr || !strings.Contains(text, `"done":true`) {
		t.Fatalf("isErr=%v %s", isErr, text)
	}
	if m.calls[0] != "DELETE /api/v1/users/"+uid+"/sessions" || m.auth != "SSWS tok" {
		t.Errorf("calls=%v auth=%q", m.calls, m.auth)
	}
	call("revoke_user_sessions", map[string]any{"user_id": uid, "revoke_oauth_tokens": true})
	if m.calls[1] != "DELETE /api/v1/users/"+uid+"/sessions?oauthTokens=true" {
		t.Errorf("calls=%v", m.calls)
	}
}

func TestSuspendAndUnsuspend(t *testing.T) {
	m, call, _ := setup(t)
	if isErr, text := call("suspend_user", map[string]any{"user_id": uid}); isErr || m.status != "SUSPENDED" {
		t.Fatalf("suspend: %v %s %s", isErr, text, m.status)
	}
	// Okta refuses a second suspend. The tool shows the Okta error.
	if isErr, text := call("suspend_user", map[string]any{"user_id": uid}); !isErr || !strings.Contains(text, "E0000001") {
		t.Errorf("second suspend: %v %s", isErr, text)
	}
	if isErr, text := call("unsuspend_user", map[string]any{"user_id": uid}); isErr || m.status != "ACTIVE" {
		t.Fatalf("unsuspend: %v %s %s", isErr, text, m.status)
	}
	if m.calls[0] != "POST /api/v1/users/"+uid+"/lifecycle/suspend" {
		t.Errorf("calls=%v", m.calls)
	}
}

func TestUnknownUser(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text := call("suspend_user", map[string]any{"user_id": "00uZZZZZZZZZZZZZZZZZ"})
	if !isErr || !strings.Contains(text, "E0000007") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "a@b.com", uid + "x", uid[:19], "../sessions/" + uid, uid + "?x=1", "00ub0oNGTSWTBKOLGL;R", strings.Repeat("a", 500)} {
		for _, tool := range []string{"revoke_user_sessions", "suspend_user", "unsuspend_user"} {
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
	kittest.AssertIrreversible(t, tools, "revoke_user_sessions")
	kittest.AssertReversible(t, tools, "suspend_user", "unsuspend_user")
	kittest.AssertReversible(t, tools, "unsuspend_user", "suspend_user")
	for name := range tools {
		kittest.AssertIdempotent(t, tools, name, true)
	}
}

func TestConfigRefusesPlainHTTP(t *testing.T) {
	_, err := build(kit.NewEnvFrom(map[string]string{"OKTA_ORG_URL": "http://x.okta.com", "OKTA_API_TOKEN": "t"}), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Error("plain http accepted")
	}
	if _, err := build(kit.NewEnvFrom(nil), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("empty environment accepted")
	}
}
