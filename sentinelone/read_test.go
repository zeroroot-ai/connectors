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

// readMock follows the public descriptions of the SentinelOne calls
// GET /web/api/v2.1/threats and GET /web/api/v2.1/agents (see read.go).
// Each answers {"data":[...],"pagination":{"nextCursor","totalItems"}}.
// A bad token gets 401 with {"errors":[{"code":4010010,...}]}.
type readMock struct {
	methods []string
	queries []string
}

const (
	threatsJS = `{"data":[{"id":"1000000000000000001","threatInfo":{"threatName":"evil.exe","classification":"Trojan",` +
		`"confidenceLevel":"malicious","analystVerdict":"true_positive","incidentStatus":"unresolved",` +
		`"mitigationStatus":"not_mitigated","createdAt":"2026-11-20T10:00:00.000000Z","filePath":"C:\\evil.exe",` +
		`"sha256":"abc","sha1":"def","md5":"ghi"},"agentRealtimeInfo":{"agentId":"225494730938493804",` +
		`"agentComputerName":"host-1","agentOsType":"windows"},"agentDetectionInfo":{"agentIpV4":"10.0.0.1"},` +
		`"indicators":[]}],"pagination":{"nextCursor":"cur123","totalItems":42}}`
	agentsJS = `{"data":[{"id":"225494730938493804","computerName":"host-1","networkStatus":"connected","isActive":true,` +
		`"isDecommissioned":false,"activeThreats":2,"agentVersion":"23.1.2.3","osName":"Windows 11","machineType":"laptop",` +
		`"externalIp":"203.0.113.5","lastActiveDate":"2026-11-20T10:00:00.000000Z","siteName":"HQ","uuid":"u","lastIpToMgmt":"x"}],` +
		`"pagination":{"nextCursor":null,"totalItems":1}}`
)

func (m *readMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.methods = append(m.methods, r.Method)
	m.queries = append(m.queries, r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "ApiToken tok" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":4010010,"title":"Authentication Failed","detail":"bad token"}]}`))
		return
	}
	switch r.URL.Path {
	case "/web/api/v2.1/threats":
		_, _ = w.Write([]byte(threatsJS))
	case "/web/api/v2.1/agents":
		_, _ = w.Write([]byte(agentsJS))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func readSetup(t *testing.T, token string) (srv *readMock, call func(string, map[string]any) (bool, string, *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &readMock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{"SENTINELONE_CONSOLE_URL": ts.URL, "SENTINELONE_API_TOKEN": token}),
		slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string, *mcp.CallToolResult) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res), res
	}, kittest.Tools(t, cs)
}

func TestListThreats(t *testing.T) {
	m, call, _ := readSetup(t, "tok")
	isErr, text, res := call("list_threats", map[string]any{
		"agent_id": agent, "incident_status": "unresolved", "created_after": "2026-11-20T00:00:00Z", "limit": 10,
	})
	if isErr {
		t.Fatal(text)
	}
	var out threatsOutput
	kittest.Structured(t, res, &out)
	if len(out.Threats) != 1 || out.Threats[0].ThreatInfo.ThreatName != "evil.exe" ||
		out.Threats[0].AgentRealtimeInfo.AgentComputerName != "host-1" || out.NextCursor != "cur123" || out.TotalItems != 42 {
		t.Errorf("out = %+v", out)
	}
	for _, want := range []string{"agentIds=" + agent, "incidentStatuses=unresolved", "limit=10", "createdAt__gte=2026-11-20T00%3A00%3A00Z"} {
		if !strings.Contains(m.queries[0], want) {
			t.Errorf("query %q lacks %q", m.queries[0], want)
		}
	}
	// A page cursor goes through, and the default limit applies.
	call("list_threats", map[string]any{"cursor": "cur123"})
	if !strings.Contains(m.queries[1], "cursor=cur123") || !strings.Contains(m.queries[1], "limit=25") {
		t.Errorf("query = %s", m.queries[1])
	}
}

func TestListAgents(t *testing.T) {
	m, call, _ := readSetup(t, "tok")
	isErr, text, res := call("list_agents", map[string]any{"computer_name": "host-1", "network_status": "connected", "is_active": true})
	if isErr {
		t.Fatal(text)
	}
	var out agentsOutput
	kittest.Structured(t, res, &out)
	if len(out.Agents) != 1 || out.Agents[0].NetworkStatus != "connected" || out.Agents[0].ActiveThreats != 2 || out.NextCursor != "" {
		t.Errorf("out = %+v", out)
	}
	for _, want := range []string{"computerName=host-1", "networkStatuses=connected", "isActive=true"} {
		if !strings.Contains(m.queries[0], want) {
			t.Errorf("query %q lacks %q", m.queries[0], want)
		}
	}
	// is_active false is a filter and not an absent input.
	call("list_agents", map[string]any{"is_active": false})
	if !strings.Contains(m.queries[1], "isActive=false") {
		t.Errorf("query = %s", m.queries[1])
	}
}

func TestReadToolsRefuseBadInput(t *testing.T) {
	m, call, _ := readSetup(t, "tok")
	bad := []map[string]any{
		{"threat_id": "abc"}, {"threat_id": agent + ",1"}, {"agent_id": "../x"}, {"incident_status": "a&b=c"},
		{"mitigation_status": "X Y"}, {"created_after": "yesterday"}, {"created_before": "2026-11-20"},
		{"limit": 101}, {"limit": -2}, {"cursor": "a b"}, {"cursor": strings.Repeat("a", 513)},
	}
	for _, in := range bad {
		if isErr, _, _ := call("list_threats", in); !isErr {
			t.Errorf("list_threats accepted %v", in)
		}
	}
	for _, in := range []map[string]any{
		{"agent_id": "x"}, {"computer_name": "a&b"}, {"computer_name": "a\nb"}, {"network_status": "Connected"}, {"limit": 0 - 1}, {"cursor": "x y"},
	} {
		if isErr, _, _ := call("list_agents", in); !isErr {
			t.Errorf("list_agents accepted %v", in)
		}
	}
	if len(m.queries) != 0 {
		t.Errorf("a bad input reached SentinelOne: %v", m.queries)
	}
}

func TestReadAuthFailureShowsNoToken(t *testing.T) {
	_, call, _ := readSetup(t, "wrongtoken")
	isErr, text, _ := call("list_agents", map[string]any{})
	if !isErr || !strings.Contains(text, "HTTP 401") || strings.Contains(text, "wrongtoken") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

// TestReadToolsOnlyRead makes sure each read tool is marked read only and
// sends only GET, and that the response tools stay marked as changes.
func TestReadToolsOnlyRead(t *testing.T) {
	m, call, tools := readSetup(t, "tok")
	for _, name := range []string{"list_threats", "list_agents"} {
		tl := tools[name]
		if tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint || !tl.Annotations.IdempotentHint {
			t.Errorf("%s is not marked read only", name)
		}
		if isErr, text, _ := call(name, map[string]any{}); isErr {
			t.Errorf("%s: %s", name, text)
		}
	}
	if len(m.methods) != 2 {
		t.Fatalf("calls = %v", m.methods)
	}
	for _, method := range m.methods {
		if method != http.MethodGet {
			t.Errorf("a read tool sent %s", method)
		}
	}
	for _, name := range []string{"disconnect_from_network", "reconnect_to_network"} {
		if tools[name].Annotations.ReadOnlyHint {
			t.Errorf("%s is marked read only", name)
		}
	}
}

// truncated says that the result is a part of the list. It is true when
// SentinelOne gives a next cursor, or when the first page has fewer items than
// the total. An agent must not take such a result for the whole list.
func TestReadsSayWhenTheListIsTruncated(t *testing.T) {
	_, call, _ := readSetup(t, "tok")
	_, _, res := call("list_threats", map[string]any{})
	var th threatsOutput
	kittest.Structured(t, res, &th)
	if !th.Truncated || th.NextCursor != "cur123" || th.TotalItems != 42 {
		t.Errorf("threats = %+v", th)
	}
	// A complete answer: no cursor, and the total equals the count.
	_, _, res = call("list_agents", map[string]any{})
	var ag agentsOutput
	kittest.Structured(t, res, &ag)
	if ag.Truncated || ag.NextCursor != "" {
		t.Errorf("agents = %+v", ag)
	}
	// A later page with no cursor is the last page, even when the total is higher.
	if got := newPage(pagination{NextCursor: "", TotalItems: 42}, 5, "cur123"); got.Truncated {
		t.Errorf("last page = %+v", got)
	}
	if got := newPage(pagination{NextCursor: "", TotalItems: 42}, 25, ""); !got.Truncated {
		t.Errorf("first page with a larger total = %+v", got)
	}
	if got := newPage(pagination{NextCursor: "c", TotalItems: 0}, 25, "x"); !got.Truncated {
		t.Errorf("a next cursor = %+v", got)
	}
}
