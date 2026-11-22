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

const (
	routing = "R0UT1NGKEY0123456789abcdef012345"
	dedup   = "finding/42:disk-full"
)

// The mock follows PagerDuty:
// the Events API v2 (POST /v2/enqueue answers 202 {"status":"success",
// "message":"Event processed","dedup_key":...}, a body with a bad field answers
// 400) and the REST API v2 (GET /incidents/{id} answers {"incident":{...}},
// GET /incidents lists with incident_key). One server answers both addresses.
type mock struct {
	calls   []string
	bodies  []map[string]any
	queries []string
	header  http.Header
}

const incidentJSON = `{"id":"PT4KHLK","incident_number":1234,"title":"Disk full","status":"triggered","urgency":"high",` +
	`"html_url":"https://acme.pagerduty.com/incidents/PT4KHLK","created_at":"2026-11-20T10:00:00Z",` +
	`"last_status_change_at":"2026-11-20T10:05:00Z","incident_key":"` + dedup + `","service":{"id":"PSVC1","summary":"Shop"},` +
	`"priority":{"summary":"P1"},"assignments":[]}`

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.queries = append(m.queries, r.URL.RawQuery)
	m.header = r.Header.Clone()
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	m.bodies = append(m.bodies, body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v2/enqueue":
		if body["routing_key"] != routing {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"invalid event","message":"Event object is invalid","errors":["routing_key"]}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success","message":"Event processed","dedup_key":"` + body["dedup_key"].(string) + `"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/incidents/PT4KHLK":
		_, _ = w.Write([]byte(`{"incident":` + incidentJSON + `}`))
	case r.Method == http.MethodGet && r.URL.Path == "/incidents":
		_, _ = w.Write([]byte(`{"incidents":[` + incidentJSON + `],"limit":10,"offset":0,"more":false}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Not Found","code":2100}}`))
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"PAGERDUTY_ROUTING_KEY": routing, "PAGERDUTY_API_TOKEN": "rest-token",
		"PAGERDUTY_EVENTS_URL": ts.URL, "PAGERDUTY_API_URL": ts.URL,
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

func TestTriggerAcknowledgeResolve(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("trigger_alert", map[string]any{
		"summary": "Disk full on db-1", "source": "db-1", "dedup_key": dedup, "severity": "critical",
		"component": "disk", "custom_details": map[string]string{"finding": "42"},
	})
	if isErr {
		t.Fatal(text)
	}
	var out eventOutput
	kittest.Structured(t, res, &out)
	if out.DedupKey != dedup || out.Action != "trigger" || out.Message != "Event processed" {
		t.Errorf("out = %+v", out)
	}
	b := m.bodies[0]
	p := b["payload"].(map[string]any)
	if b["routing_key"] != routing || b["event_action"] != "trigger" || b["dedup_key"] != dedup ||
		p["summary"] != "Disk full on db-1" || p["source"] != "db-1" || p["severity"] != "critical" || p["component"] != "disk" ||
		p["custom_details"].(map[string]any)["finding"] != "42" {
		t.Errorf("body = %v", b)
	}
	if isErr, text, _ := call("acknowledge_alert", map[string]any{"dedup_key": dedup}); isErr {
		t.Fatal(text)
	}
	if isErr, text, _ := call("resolve_alert", map[string]any{"dedup_key": dedup}); isErr {
		t.Fatal(text)
	}
	if b := m.bodies[1]; b["event_action"] != "acknowledge" || b["dedup_key"] != dedup || b["payload"] != nil {
		t.Errorf("acknowledge body = %v", b)
	}
	if b := m.bodies[2]; b["event_action"] != "resolve" || b["dedup_key"] != dedup {
		t.Errorf("resolve body = %v", b)
	}
	// The default severity is error.
	call("trigger_alert", map[string]any{"summary": "x", "source": "y", "dedup_key": "k1"})
	if p := m.bodies[3]["payload"].(map[string]any); p["severity"] != "error" {
		t.Errorf("default severity = %v", p["severity"])
	}
}

func TestTriggerRefusesBadInput(t *testing.T) {
	m, call, _ := setup(t)
	ok := func() map[string]any { return map[string]any{"summary": "s", "source": "x", "dedup_key": "k"} }
	for name, f := range map[string]func(map[string]any){
		"no dedup key":   func(a map[string]any) { delete(a, "dedup_key") },
		"long dedup key": func(a map[string]any) { a["dedup_key"] = strings.Repeat("a", 256) },
		"dedup newline":  func(a map[string]any) { a["dedup_key"] = "a\nb" },
		"dedup space":    func(a map[string]any) { a["dedup_key"] = "a b" },
		"no summary":     func(a map[string]any) { a["summary"] = "" },
		"long summary":   func(a map[string]any) { a["summary"] = strings.Repeat("a", 1025) },
		"no source":      func(a map[string]any) { a["source"] = "" },
		"bad severity":   func(a map[string]any) { a["severity"] = "fatal" },
		"bad detail key": func(a map[string]any) { a["custom_details"] = map[string]string{"a\nb": "x"} },
		"many details": func(a map[string]any) {
			d := map[string]string{}
			for i := range 21 {
				d[strings.Repeat("k", i+1)] = "v"
			}
			a["custom_details"] = d
		},
	} {
		a := ok()
		f(a)
		if isErr, _, _ := call("trigger_alert", a); !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, tool := range []string{"acknowledge_alert", "resolve_alert", "find_incidents"} {
		for _, bad := range []string{"", "a b", strings.Repeat("a", 256), "a\nb"} {
			if isErr, _, _ := call(tool, map[string]any{"dedup_key": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached PagerDuty: %v", m.calls)
	}
}

func TestEventErrorShowsPagerDutyAnswerAndNoKey(t *testing.T) {
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	defer ts.Close()
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"PAGERDUTY_ROUTING_KEY": "WR0NGKEY0123456789abcdef01234567", "PAGERDUTY_API_TOKEN": "t",
		"PAGERDUTY_EVENTS_URL": ts.URL, "PAGERDUTY_API_URL": ts.URL,
	}), slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	res := kittest.Call(t, kittest.Connect(t, register), "resolve_alert", map[string]any{"dedup_key": "k"})
	text := kittest.Text(t, res)
	if !res.IsError || !strings.Contains(text, "HTTP 400") || strings.Contains(text, "WR0NGKEY") || strings.Contains(text, routing) {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
}

func TestGetAndFindIncident(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("get_incident", map[string]any{"incident_id": "PT4KHLK"})
	if isErr {
		t.Fatal(text)
	}
	var out incidentOut
	kittest.Structured(t, res, &out)
	if out.ID != "PT4KHLK" || out.Number != 1234 || out.Status != "triggered" || out.Urgency != "high" || out.DedupKey != dedup ||
		out.Service != "Shop" || out.ServiceID != "PSVC1" || out.Priority != "P1" || !strings.Contains(out.URL, "PT4KHLK") {
		t.Errorf("out = %+v", out)
	}
	if got := m.header.Get("Authorization"); got != "Token token=rest-token" {
		t.Errorf("auth = %q", got)
	}
	if got := m.header.Get("Accept"); got != "application/vnd.pagerduty+json;version=2" {
		t.Errorf("accept = %q", got)
	}
	_, _, res = call("find_incidents", map[string]any{"dedup_key": dedup})
	var f findOutput
	kittest.Structured(t, res, &f)
	if f.Count != 1 || f.Incidents[0].ID != "PT4KHLK" {
		t.Errorf("find = %+v", f)
	}
	if q := m.queries[1]; !strings.Contains(q, "incident_key=finding%2F42%3Adisk-full") || !strings.Contains(q, "date_range=all") {
		t.Errorf("query = %s", q)
	}
	if isErr, text, _ := call("get_incident", map[string]any{"incident_id": "PNOPE00"}); !isErr || !strings.Contains(text, "HTTP 404") {
		t.Errorf("unknown incident: isErr=%v %s", isErr, text)
	}
}

func TestIncidentIDValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "pt4khlk", "PT4KHLK/notes", "../x", "PT4KHLK?x=1", strings.Repeat("A", 21), "A B"} {
		if isErr, _, _ := call("get_incident", map[string]any{"incident_id": bad}); !isErr {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad id reached PagerDuty: %v", m.calls)
	}
}

func TestAnnotationsAndReadOnly(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 5 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, "trigger_alert", "resolve_alert")
	kittest.AssertIrreversible(t, tools, "acknowledge_alert")
	kittest.AssertIrreversible(t, tools, "resolve_alert")
	for _, n := range []string{"get_incident", "find_incidents"} {
		if a := tools[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is not read only", n)
		}
	}
	call("get_incident", map[string]any{"incident_id": "PT4KHLK"})
	call("find_incidents", map[string]any{"dedup_key": "k"})
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
	for name, env := range map[string]map[string]string{
		"short routing key": {"PAGERDUTY_ROUTING_KEY": "abc", "PAGERDUTY_API_TOKEN": "t"},
		"plain http":        {"PAGERDUTY_ROUTING_KEY": routing, "PAGERDUTY_API_TOKEN": "t", "PAGERDUTY_EVENTS_URL": "http://events.pagerduty.com"},
	} {
		if _, err := build(kit.NewEnvFrom(env), log); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := build(kit.NewEnvFrom(map[string]string{"PAGERDUTY_ROUTING_KEY": routing, "PAGERDUTY_API_TOKEN": "t"}), log); err != nil {
		t.Errorf("good config: %v", err)
	}
}
