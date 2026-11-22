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

const agent = "225494730938493804"

// The mock follows the calls of public clients of the SentinelOne Management
// API (the PowerShell module PS-SentinelOne and the extension of LimaCharlie):
// POST /web/api/v2.1/agents/actions/{disconnect,connect} with
// {"filter":{"ids":[...]},"data":{}} answers {"data":{"affected":n}}.
// An unknown id matches no agent, so "affected" is 0. A bad token gets 401
// with {"errors":[{"code":4010010,"title":"Authentication Failed", ...}]}.
type mock struct {
	calls        []string
	bodies       []string
	disconnected bool
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := io.ReadAll(r.Body)
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.bodies = append(m.bodies, string(b))
	if r.Header.Get("Authorization") != "ApiToken tok" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":4010010,"title":"Authentication Failed","detail":"bad token"}]}`))
		return
	}
	var body struct {
		Filter struct {
			IDs []string `json:"ids"`
		} `json:"filter"`
	}
	_ = json.Unmarshal(b, &body)
	affected := 0
	if len(body.Filter.IDs) == 1 && body.Filter.IDs[0] == agent {
		switch r.URL.Path {
		case "/web/api/v2.1/agents/actions/disconnect":
			if !m.disconnected {
				affected = 1
			}
			m.disconnected = true
		case "/web/api/v2.1/agents/actions/connect":
			if m.disconnected {
				affected = 1
			}
			m.disconnected = false
		}
	}
	_, _ = w.Write([]byte(`{"data":{"affected":` + string(rune('0'+affected)) + `}}`))
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (bool, string), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"SENTINELONE_CONSOLE_URL": ts.URL, "SENTINELONE_API_TOKEN": "tok",
	}), slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}, kittest.Tools(t, cs)
}

func TestDisconnectAndReconnect(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text := call("disconnect_from_network", map[string]any{"agent_id": agent})
	if isErr || !strings.Contains(text, `"affected":1`) || !m.disconnected {
		t.Fatalf("disconnect: %v %s", isErr, text)
	}
	if m.calls[0] != "POST /web/api/v2.1/agents/actions/disconnect" || m.bodies[0] != `{"data":{},"filter":{"ids":["`+agent+`"]}}` {
		t.Errorf("calls=%v bodies=%v", m.calls, m.bodies)
	}
	// A second disconnect changes nothing. The tool says so and is not an error.
	isErr, text = call("disconnect_from_network", map[string]any{"agent_id": agent})
	if isErr || !strings.Contains(text, `"affected":0`) || !strings.Contains(text, "changed no agent") {
		t.Errorf("second disconnect: %v %s", isErr, text)
	}
	isErr, text = call("reconnect_to_network", map[string]any{"agent_id": agent})
	if isErr || m.disconnected || !strings.Contains(text, `"affected":1`) {
		t.Fatalf("reconnect: %v %s", isErr, text)
	}
}

func TestUnknownAgentChangesNothing(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text := call("disconnect_from_network", map[string]any{"agent_id": "999999999999999999"})
	if isErr || !strings.Contains(text, `"affected":0`) {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestAuthFailureShowsVendorError(t *testing.T) {
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	defer ts.Close()
	register, _ := buildWith(kit.NewEnvFrom(map[string]string{"SENTINELONE_CONSOLE_URL": ts.URL, "SENTINELONE_API_TOKEN": "wrong"}),
		slog.New(slog.DiscardHandler), ts.Client())
	cs := kittest.Connect(t, register)
	res := kittest.Call(t, cs, "disconnect_from_network", map[string]any{"agent_id": agent})
	text := kittest.Text(t, res)
	if !res.IsError || !strings.Contains(text, "Authentication Failed") || strings.Contains(text, "wrong") {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "host-1", agent + "x", "123", agent + ",111111111111111111", "-" + agent, "1e18", agent + "\n"} {
		for _, tool := range []string{"disconnect_from_network", "reconnect_to_network"} {
			if isErr, _ := call(tool, map[string]any{"agent_id": bad}); !isErr {
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
	if len(tools) != 4 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, "disconnect_from_network", "reconnect_to_network")
	kittest.AssertReversible(t, tools, "reconnect_to_network", "disconnect_from_network")
	kittest.AssertIdempotent(t, tools, "disconnect_from_network", true)
	kittest.AssertIdempotent(t, tools, "reconnect_to_network", true)
}

func TestConfigRefusesPlainHTTP(t *testing.T) {
	_, err := build(kit.NewEnvFrom(map[string]string{"SENTINELONE_CONSOLE_URL": "http://x.sentinelone.net", "SENTINELONE_API_TOKEN": "t"}),
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Error("plain http accepted")
	}
}
