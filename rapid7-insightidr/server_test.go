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

const rrn = "rrn:investigation:us1:00000000-0000-0000-0000-000000000000:investigation:6A74T2A4"

// The mock follows the InsightIDR API v2
// (https://help.rapid7.com/insightidr/en-us/api/v2/insightidr-api-v2.json):
// a list answer is {"data":[...],"metadata":{"index","size","total_pages","total_data"}},
// an investigation has rrn, title, source, status, priority, disposition,
// assignee, created_time and the alert times, and an alert has id, title,
// alertType, alertSource, detectionRuleRrn and the event times.
type mock struct {
	calls   []string
	queries []url.Values
	key     string
}

const invJS = `{"rrn":"` + rrn + `","title":"Suspicious login","source":"ALERT","status":"OPEN","priority":"HIGH","disposition":"UNDECIDED",` +
	`"assignee":{"email":"a@example.com","name":"A"},"created_time":"2026-11-20T10:00:00Z","last_accessed":"2026-11-20T11:00:00Z",` +
	`"first_alert_time":"2026-11-20T09:00:00Z","latest_alert_time":"2026-11-20T09:30:00Z","tags":["x"]}`

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.calls = append(m.calls, r.Method+" "+r.URL.EscapedPath())
	m.queries = append(m.queries, r.URL.Query())
	m.key = r.Header.Get("X-Api-Key")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/idr/v2/investigations":
		_, _ = w.Write([]byte(`{"data":[` + invJS + `],"metadata":{"index":0,"size":20,"total_pages":2,"total_data":30}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/idr/v2/investigations/"+rrn:
		_, _ = w.Write([]byte(invJS))
	case r.Method == http.MethodGet && r.URL.Path == "/idr/v2/investigations/"+rrn+"/alerts":
		_, _ = w.Write([]byte(`{"data":[{"id":"alert-1","title":"Impossible travel","alertType":"IMPOSSIBLE_TRAVEL","alertTypeDescription":"A user signed in from two places",` +
			`"alertSource":"INSIGHT_IDR","detectionRuleRrn":{"ruleName":"Impossible Travel","ruleRrn":{"partition":"cba","service":"","regionCode":"","organizationId":"","resourceTypes":["detection-rule"],"resource":"FX11KBBSCK20"}},"createdTime":"2026-11-20T09:31:00Z","firstEventTime":"2026-11-20T09:00:00Z",` +
			`"latestEventTime":"2026-11-20T09:30:00Z"}],"metadata":{"index":0,"size":20,"total_pages":1,"total_data":1}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"message":"Not found"}`))
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{"INSIGHTIDR_API_KEY": "apikey-123", "INSIGHTIDR_URL": ts.URL}),
		slog.New(slog.DiscardHandler), ts.Client())
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

func TestListInvestigations(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("list_investigations", map[string]any{
		"status": "OPEN", "priority": "HIGH", "source": "ALERT", "start_time": "2026-11-01T00:00:00Z", "end_time": "2026-11-21T00:00:00Z", "size": 20,
	})
	if isErr {
		t.Fatal(text)
	}
	var out listInvestigationsOut
	kittest.Structured(t, res, &out)
	if len(out.Investigations) != 1 || out.Investigations[0].ID != rrn || out.Investigations[0].Status != "OPEN" || out.Investigations[0].Assignee != "a@example.com" ||
		out.TotalRecords != 30 || out.TotalPages != 2 {
		t.Errorf("out = %+v", out)
	}
	q := m.queries[0]
	if q.Get("statuses") != "OPEN" || q.Get("priorities") != "HIGH" || q.Get("sources") != "ALERT" || q.Get("start_time") != "2026-11-01T00:00:00Z" ||
		q.Get("end_time") != "2026-11-21T00:00:00Z" || q.Get("size") != "20" || q.Get("index") != "0" {
		t.Errorf("query = %v", q)
	}
	if m.key != "apikey-123" {
		t.Errorf("key = %q", m.key)
	}
}

func TestGetInvestigationAndAlerts(t *testing.T) {
	m, call, _ := setup(t)
	_, _, res := call("get_investigation", map[string]any{"investigation_id": rrn})
	var inv investigationOut
	kittest.Structured(t, res, &inv)
	if inv.ID != rrn || inv.Title != "Suspicious login" || inv.Priority != "HIGH" || inv.LatestAlertTime != "2026-11-20T09:30:00Z" {
		t.Errorf("investigation = %+v", inv)
	}
	_, _, res = call("list_investigation_alerts", map[string]any{"investigation_id": rrn, "size": 5, "index": 1})
	var al alertsOut
	kittest.Structured(t, res, &al)
	if len(al.Alerts) != 1 || al.Alerts[0].ID != "alert-1" || al.Alerts[0].AlertType != "IMPOSSIBLE_TRAVEL" || al.Alerts[0].DetectionRuleRRN != "rrn:cba::::detection-rule:FX11KBBSCK20" || al.Alerts[0].DetectionRuleName != "Impossible Travel" {
		t.Errorf("alerts = %+v", al)
	}
	if q := m.queries[1]; q.Get("size") != "5" || q.Get("index") != "1" {
		t.Errorf("query = %v", q)
	}
	if isErr, text, _ := call("get_investigation", map[string]any{"investigation_id": "rrn:investigation:x"}); !isErr || !strings.Contains(text, "HTTP 404") {
		t.Errorf("unknown: isErr=%v %s", isErr, text)
	}
}

func TestAssigneeAsText(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{{"a@b.c", "a@b.c"}, {map[string]any{"email": "x@y.z"}, "x@y.z"}, {nil, ""}, {5.0, ""}} {
		if got := assigneeText(c.in); got != c.want {
			t.Errorf("assigneeText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReadsRefuseBadInput(t *testing.T) {
	m, call, _ := setup(t)
	for _, in := range []map[string]any{
		{"status": "open"}, {"status": "OPEN,CLOSED"}, {"priority": "URGENT"}, {"source": "x&y=1"}, {"start_time": "yesterday"},
		{"end_time": "2026-11-20"}, {"size": 101}, {"size": -1}, {"index": -1},
	} {
		if isErr, _, _ := call("list_investigations", in); !isErr {
			t.Errorf("list_investigations accepted %v", in)
		}
	}
	for _, tool := range []string{"get_investigation", "list_investigation_alerts"} {
		for _, bad := range []string{"", "a b", "../x", "a/b", "a?x=1", strings.Repeat("a", 201), "a\nb"} {
			if isErr, _, _ := call(tool, map[string]any{"investigation_id": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached InsightIDR: %v", m.calls)
	}
}

func TestReadOnlyAndKeyNotShown(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 3 {
		t.Fatalf("tools = %d", len(tools))
	}
	for n, tl := range tools {
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not read only", n)
		}
	}
	call("list_investigations", map[string]any{})
	call("get_investigation", map[string]any{"investigation_id": rrn})
	call("list_investigation_alerts", map[string]any{"investigation_id": rrn})
	for _, c := range m.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("a read tool sent %s", c)
		}
	}
	_, text, _ := call("get_investigation", map[string]any{"investigation_id": "nope"})
	if strings.Contains(text, "apikey-123") {
		t.Errorf("an error shows the key: %s", text)
	}
}

func TestConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := build(kit.NewEnvFrom(nil), log); err == nil {
		t.Error("empty environment accepted")
	}
	for name, env := range map[string]map[string]string{
		"bad region": {"INSIGHTIDR_API_KEY": "k", "INSIGHTIDR_REGION": "us.evil.example/"},
		"plain http": {"INSIGHTIDR_API_KEY": "k", "INSIGHTIDR_URL": "http://us.api.insight.rapid7.com"},
	} {
		if _, err := build(kit.NewEnvFrom(env), log); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := build(kit.NewEnvFrom(map[string]string{"INSIGHTIDR_API_KEY": "k", "INSIGHTIDR_REGION": "eu"}), log); err != nil {
		t.Errorf("good config: %v", err)
	}
}

// The spec types detectionRuleRrn as an object {ruleName, ruleRrn}, and ruleRrn
// as an object too. The examples show a text. The tool takes both.
func TestRRNText(t *testing.T) {
	if rrnText("rrn:a:b") != "rrn:a:b" || rrnText(nil) != "" || rrnText(5.0) != "" {
		t.Error("rrnText of a text, nil or a number")
	}
	if got := rrnText(map[string]any{"partition": "cba", "service": "", "regionCode": "", "organizationId": "", "resourceTypes": []any{"detection-rule"}, "resource": "FX1"}); got != "rrn:cba::::detection-rule:FX1" {
		t.Errorf("rrnText of an object = %q", got)
	}
}
