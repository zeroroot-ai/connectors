package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

const agent = "ADB3C24C-232B-11EF-3D71-005056AC48D2"

// The mock follows the Trellix pages
// https://docs.trellix.com/docs/get-alerts-1.md and
// https://docs.trellix.com/docs/get-host-info-1.md: a JSON:API answer with
// data[].attributes, and the headers Authorization, x-api-key and
// Content-Type: application/vnd.api+json. The token endpoint is OAuth 2.0
// client credentials with the client id and secret in the authorization header.
type mock struct {
	calls      []string
	queries    []url.Values
	headers    []http.Header
	tokenUser  string
	tokenForm  url.Values
	tokenCalls int
	alerts     string
}

const alertsJS = `{"jsonapi":{"version":"1.0"},"meta":{},"data":[{"type":"alerts","id":"01c0.fb76","attributes":{` +
	`"Trace_Id":"a533","DetectionDate":"2025-11-10T05:50:41.464+00:00","Event_Date":"2025-11-10T05:45:01.000Z","Activity":"Threat Detected",` +
	`"Severity":"s0","Score":30,"Rank":30,"RuleId":"_process_file_remove","Artifact":"Threat","Host_Name":"82ZSUSE15SP6","Host_OS":"linux",` +
	`"MAGUID":"` + agent + `","ProcessName":"run-crons","CommandLine":"/usr/lib/cron/run-crons","Detection_Tags":["@ATA.DefenseEvasion","@ATE.T1070.004"],` +
	`"User":{"domain":"","name":"root"},"HostInfo":"{ifaces=[]}","Process_Sha256":"81b4"}}],` +
	`"links":{"self":"/edr/v3/alerts?page[offset]=0&page[limit]=1","next":"/edr/v3/alerts?page[offset]=1&page[limit]=1"}}`

const hostsJS = `{"jsonapi":{"version":"1.0"},"meta":{"totalResourceCount":3},"data":[{"type":"hostInfo","attributes":{` +
	`"aGuid":"` + agent + `","hostname":"3M4W10RS5X64","os":{"major":10,"minor":0,"build":17763,"sp":"","desc":"Windows 10"},` +
	`"lastBootTime":"2024-10-10T11:51:04Z","netInterfaces":[{"name":"Ethernet0","macAddress":"00:50:56:a4:31:d9","mac":"00:50:56:a4:31:d9","ip":"10.26.22.121","type":6}],` +
	`"traceExtendedVisibility":0,"hostOs":"windows"}}],"links":{"next":"/x"}}`

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/iam/v1.1/token" {
		m.tokenCalls++
		m.tokenUser, _, _ = r.BasicAuth()
		_ = r.ParseForm()
		m.tokenForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"trellix-token","token_type":"Bearer","expires_in":3600}`))
		return
	}
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.queries = append(m.queries, r.URL.Query())
	m.headers = append(m.headers, r.Header.Clone())
	if r.Header.Get("Authorization") != "Bearer trellix-token" || r.Header.Get("x-api-key") != "key123" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"status":"401"}]}`))
		return
	}
	switch r.URL.Path {
	case "/edr/v3/alerts":
		_, _ = w.Write([]byte(m.alerts))
	case "/edr/v2/remediation/host-info":
		_, _ = w.Write([]byte(hostsJS))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{alerts: alertsJS}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"TRELLIX_CLIENT_ID": "cid", "TRELLIX_CLIENT_SECRET": "csecret", "TRELLIX_API_KEY": "key123",
		"TRELLIX_API_URL": ts.URL, "TRELLIX_TOKEN_URL": ts.URL + "/iam/v1.1/token",
	}), slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	call = func(tool string, args map[string]any) (bool, string, *mcp.CallToolResult) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res), res
	}
	return m, call, kittest.Tools(t, cs)
}

func TestListAlerts(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("list_alerts", map[string]any{"since": "2026-11-20T10:00:00Z", "until": "2026-11-21T10:00:00Z", "limit": 1})
	if isErr {
		t.Fatal(text)
	}
	var out alertsOutput
	kittest.Structured(t, res, &out)
	if out.Count != 1 || !out.HasMore || out.Alerts[0].RuleID != "_process_file_remove" || out.Alerts[0].Severity != "s0" ||
		out.Alerts[0].HostName != "82ZSUSE15SP6" || out.Alerts[0].AgentGUID != agent || out.Alerts[0].User != "root" || len(out.Alerts[0].Tags) != 2 {
		t.Errorf("out = %+v", out)
	}
	q := m.queries[0]
	if q.Get("from") != "1795168800000" || q.Get("to") != "1795255200000" || q.Get("page[limit]") != "1" || q.Get("page[offset]") != "0" {
		t.Errorf("query = %v", q)
	}
	if m.calls[0] != "GET /edr/v3/alerts" {
		t.Errorf("calls = %v", m.calls)
	}
	h := m.headers[0]
	if h.Get("x-api-key") != "key123" || h.Get("Content-Type") != "application/vnd.api+json" || h.Get("Authorization") != "Bearer trellix-token" {
		t.Errorf("headers = %v", h)
	}
	// The token request: client credentials, the id and the secret in the authorization header, the scopes.
	if m.tokenUser != "cid" || m.tokenForm.Get("grant_type") != "client_credentials" || m.tokenForm.Get("scope") != defaultScopes {
		t.Errorf("token: user %q form %v", m.tokenUser, m.tokenForm)
	}
	if m.tokenForm.Get("client_secret") != "" {
		t.Error("the secret went in the body")
	}
}

func TestListAlertsPagingFlag(t *testing.T) {
	m, call, _ := setup(t)
	m.alerts = `{"meta":{},"data":[]}`
	_, _, res := call("list_alerts", map[string]any{})
	var out alertsOutput
	kittest.Structured(t, res, &out)
	if out.Count != 0 || out.HasMore || out.Alerts == nil {
		t.Errorf("out = %+v", out)
	}
	if m.queries[0].Get("page[limit]") != "25" {
		t.Errorf("default limit: %v", m.queries[0])
	}
}

func TestListHosts(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("list_hosts", map[string]any{"host_name": "3M4W", "limit": 1})
	if isErr {
		t.Fatal(text)
	}
	var out hostsOutput
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Total != 3 || !out.HasMore || out.Hosts[0].AgentGUID != agent || out.Hosts[0].OSDesc != "Windows 10" ||
		out.Hosts[0].HostOS != "windows" || len(out.Hosts[0].Interfaces) != 1 || out.Hosts[0].Interfaces[0].IP != "10.26.22.121" {
		t.Errorf("out = %+v", out)
	}
	q := m.queries[0]
	if q.Get("hostName") != "3M4W" || q.Get("page[limit]") != "1" || m.calls[0] != "GET /edr/v2/remediation/host-info" {
		t.Errorf("query = %v calls = %v", q, m.calls)
	}
	if isErr, _, _ := call("list_hosts", map[string]any{"agent_guid": agent, "offset": 2}); isErr {
		t.Error("agent_guid refused")
	}
	if m.queries[1].Get("aGuid") != agent {
		t.Errorf("query = %v", m.queries[1])
	}
}

func TestReadsRefuseBadInput(t *testing.T) {
	m, call, _ := setup(t)
	for _, in := range []map[string]any{{"since": "yesterday"}, {"until": "2026-11-20"}, {"limit": 101}, {"limit": -1}, {"offset": -1}, {"offset": 100001}} {
		if isErr, _, _ := call("list_alerts", in); !isErr {
			t.Errorf("list_alerts accepted %v", in)
		}
	}
	for _, in := range []map[string]any{
		{"agent_guid": "x"}, {"agent_guid": agent + "&a=b"}, {"host_name": "a b"}, {"host_name": "a&x=1"}, {"host_name": "../x"}, {"limit": 0 - 5},
	} {
		if isErr, _, _ := call("list_hosts", in); !isErr {
			t.Errorf("list_hosts accepted %v", in)
		}
	}
	if len(m.calls) != 0 || m.tokenCalls != 0 {
		t.Errorf("a bad input reached Trellix: %v token calls %d", m.calls, m.tokenCalls)
	}
}

func TestAuthFailureShowsNoSecret(t *testing.T) {
	m := &mock{alerts: alertsJS}
	ts := httptest.NewTLSServer(m)
	defer ts.Close()
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"TRELLIX_CLIENT_ID": "cid", "TRELLIX_CLIENT_SECRET": "csecret", "TRELLIX_API_KEY": "wrong-key",
		"TRELLIX_API_URL": ts.URL, "TRELLIX_TOKEN_URL": ts.URL + "/iam/v1.1/token",
	}), slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	res := kittest.Call(t, kittest.Connect(t, register), "list_alerts", map[string]any{})
	text := kittest.Text(t, res)
	if !res.IsError || !strings.Contains(text, "HTTP 401") || strings.Contains(text, "wrong-key") || strings.Contains(text, "csecret") {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
}

// TestNoWriteTools makes sure that the server has read tools only, and that
// each one sends only GET.
func TestNoWriteTools(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	for name, tl := range tools {
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not read only", name)
		}
	}
	call("list_alerts", map[string]any{})
	call("list_hosts", map[string]any{})
	for _, c := range m.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("a read tool sent %s", c)
		}
	}
}

func TestConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := build(kit.NewEnvFrom(nil), log); err == nil {
		t.Error("empty environment accepted")
	}
	base := map[string]string{"TRELLIX_CLIENT_ID": "c", "TRELLIX_CLIENT_SECRET": "s", "TRELLIX_API_KEY": "k"}
	if _, err := build(kit.NewEnvFrom(base), log); err != nil {
		t.Errorf("good config: %v", err)
	}
	for name, extra := range map[string]map[string]string{
		"plain api":   {"TRELLIX_API_URL": "http://api.manage.trellix.com"},
		"plain token": {"TRELLIX_TOKEN_URL": "http://iam.mcafee-cloud.com/iam/v1.1/token"},
		"bad scopes":  {"TRELLIX_SCOPES": "a b&c=d"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		if _, err := build(kit.NewEnvFrom(env), log); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
