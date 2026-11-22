package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
	"github.com/zeroroot-ai/connectors/internal/ticket"
)

const (
	sysID = "9de5069c5afe602b2d0f3a2b1c5d4e6f"
	group = "d625dccec0a8016700a222a0f7900d06"
)

// The mock follows the Table API of ServiceNow
// (https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html):
// the answer is {"result": ...}, create answers 201, update answers 200 with
// the record, and a missing record answers 404. Each field is a string.
type mock struct {
	calls   []string
	queries []url.Values
	bodies  []map[string]any
	auth    string
	state   string // the state that the incident has
	ignore  bool   // answer 200 and keep the old state, as a business rule can
}

func recordJSON(state, prio string) string {
	return `{"sys_id":"` + sysID + `","number":"INC0010001","short_description":"Disk full","description":"d",` +
		`"state":"` + state + `","priority":"` + prio + `","opened_at":"2026-11-20 10:00:00","sys_updated_on":"2026-11-20 11:00:00","closed_at":""}`
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.queries = append(m.queries, r.URL.Query())
	m.auth = r.Header.Get("Authorization")
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	m.bodies = append(m.bodies, body)
	const tbl = "/api/now/table/incident"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == tbl:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":` + recordJSON("1", "3") + `}`))
	case r.Method == http.MethodGet && r.URL.Path == tbl:
		_, _ = w.Write([]byte(`{"result":[` + recordJSON("1", "3") + `]}`))
	case r.URL.Path == tbl+"/"+sysID && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"result":` + recordJSON(m.state, "2") + `}`))
	case r.URL.Path == tbl+"/"+sysID && r.Method == http.MethodPatch:
		if s, ok := body["state"].(string); ok && !m.ignore {
			m.state = s
		}
		_, _ = w.Write([]byte(`{"result":` + recordJSON(m.state, "3") + `}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"No Record found","detail":"Record doesn't exist or ACL restricts the record retrieval"},"status":"failure"}`))
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{state: "1"}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"SERVICENOW_INSTANCE_URL": ts.URL, "SERVICENOW_USER": "svc", "SERVICENOW_PASSWORD": "s3cret",
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

func TestCreateTicket(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call(ticket.ToolCreate, map[string]any{
		"title": "Disk full", "description": "line 1\nline 2", "priority": "high", "queue": group,
	})
	if isErr {
		t.Fatal(text)
	}
	var out ticket.Created
	kittest.Structured(t, res, &out)
	if out.TicketID != sysID || out.Number != "INC0010001" || !strings.HasPrefix(out.URL, "https://") || !strings.HasSuffix(out.URL, "/incident.do?sys_id="+sysID) {
		t.Errorf("out = %+v", out)
	}
	b := m.bodies[0]
	if b["short_description"] != "Disk full" || b["description"] != "line 1\nline 2" || b["urgency"] != "1" || b["impact"] != "2" || b["assignment_group"] != group {
		t.Errorf("body = %v", b)
	}
	if !strings.HasPrefix(m.auth, "Basic ") {
		t.Errorf("auth = %q", m.auth)
	}
	// The default priority is medium.
	call(ticket.ToolCreate, map[string]any{"title": "a", "description": "b"})
	if b := m.bodies[1]; b["urgency"] != "2" || b["impact"] != "2" || b["assignment_group"] != nil {
		t.Errorf("default body = %v", b)
	}
}

func TestCreateRefusesBadInput(t *testing.T) {
	m, call, _ := setup(t)
	for name, in := range map[string]map[string]any{
		"labels":       {"title": "a", "description": "b", "labels": []string{"x"}},
		"bad queue":    {"title": "a", "description": "b", "queue": "../x"},
		"no title":     {"description": "b"},
		"bad priority": {"title": "a", "description": "b", "priority": "p1"},
	} {
		if isErr, _, _ := call(ticket.ToolCreate, in); !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached ServiceNow: %v", m.calls)
	}
}

func TestUpdateCommentClose(t *testing.T) {
	m, call, _ := setup(t)
	if isErr, text, _ := call(ticket.ToolUpdate, map[string]any{"ticket_id": sysID, "title": "New", "priority": "critical"}); isErr {
		t.Fatal(text)
	}
	if b := m.bodies[0]; b["short_description"] != "New" || b["urgency"] != "1" || b["impact"] != "1" || b["description"] != nil {
		t.Errorf("update body = %v", b)
	}
	if isErr, text, _ := call(ticket.ToolComment, map[string]any{"ticket_id": sysID, "body": "checked"}); isErr {
		t.Fatal(text)
	}
	if b := m.bodies[1]; b["work_notes"] != "checked" || len(b) != 1 {
		t.Errorf("comment body = %v", b)
	}
	isErr, text, res := call(ticket.ToolClose, map[string]any{"ticket_id": sysID, "resolution": "patched"})
	if isErr {
		t.Fatal(text)
	}
	if b := m.bodies[2]; b["state"] != "7" || b["close_code"] != "Solved (Permanently)" || b["close_notes"] != "patched" {
		t.Errorf("close body = %v", b)
	}
	var out ticket.Changed
	kittest.Structured(t, res, &out)
	if out.Status != "7" || out.Action != ticket.ToolClose {
		t.Errorf("out = %+v", out)
	}
	for _, c := range m.calls {
		if !strings.HasPrefix(c, "PATCH ") {
			t.Errorf("call %s", c)
		}
	}
}

// TestCloseDetectsAnIgnoredState makes sure that a 200 with the old state is
// an error, because a business rule can drop the state silently.
func TestCloseDetectsAnIgnoredState(t *testing.T) {
	m, call, _ := setup(t)
	m.ignore = true
	if isErr, text, _ := call(ticket.ToolClose, map[string]any{"ticket_id": sysID, "resolution": "x"}); !isErr || !strings.Contains(text, "did not close") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestGetTicket(t *testing.T) {
	m, call, _ := setup(t)
	m.state = "7"
	isErr, text, res := call(ticket.ToolGet, map[string]any{"ticket_id": sysID})
	if isErr {
		t.Fatal(text)
	}
	var out ticket.Ticket
	kittest.Structured(t, res, &out)
	if out.TicketID != sysID || out.Number != "INC0010001" || out.Title != "Disk full" || out.Status != "7" || !out.Closed ||
		out.Priority != ticket.High || out.CreatedAt != "2026-11-20T10:00:00Z" || out.UpdatedAt != "2026-11-20T11:00:00Z" {
		t.Errorf("out = %+v", out)
	}
	m.state = "2"
	_, _, res = call(ticket.ToolGet, map[string]any{"ticket_id": sysID})
	var open ticket.Ticket
	kittest.Structured(t, res, &open)
	if open.Closed {
		t.Error("state 2 is closed")
	}
	if isErr, text, _ := call(ticket.ToolGet, map[string]any{"ticket_id": strings.Repeat("a", 32)}); !isErr || !strings.Contains(text, "No Record found") {
		t.Errorf("missing record: isErr=%v %s", isErr, text)
	}
}

func TestSearchBuildsTheQueryFromTypedFilters(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call(ticket.ToolSearch, map[string]any{
		"queue": group, "title_contains": "disk full", "created_after": "2026-11-20T10:00:00+02:00", "limit": 5,
	})
	if isErr {
		t.Fatal(text)
	}
	q := m.queries[0]
	want := "active=true^assignment_group=" + group + "^short_descriptionLIKEdisk full^opened_at>=2026-11-20 08:00:00^ORDERBYDESCopened_at"
	if q.Get("sysparm_query") != want || q.Get("sysparm_limit") != "5" {
		t.Errorf("query = %q limit %q", q.Get("sysparm_query"), q.Get("sysparm_limit"))
	}
	var out ticket.SearchOutput
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Tickets[0].TicketID != sysID {
		t.Errorf("out = %+v", out)
	}
	call(ticket.ToolSearch, map[string]any{"status": "any"})
	if got := m.queries[1].Get("sysparm_query"); got != "ORDERBYDESCopened_at" {
		t.Errorf("status any: %q", got)
	}
	call(ticket.ToolSearch, map[string]any{"status": "closed"})
	if got := m.queries[2].Get("sysparm_query"); !strings.HasPrefix(got, "active=false^") {
		t.Errorf("status closed: %q", got)
	}
}

// TestSearchRefusesAQueryInjection makes sure that text from a finding cannot
// add a condition to the encoded query.
func TestSearchRefusesAQueryInjection(t *testing.T) {
	m, call, _ := setup(t)
	for _, in := range []map[string]any{
		{"title_contains": "x^active=false"},
		{"title_contains": "x^ORDERBYsys_id"},
		{"queue": group + "^active=false"},
		{"queue": "a^b"},
		{"label": "x"},
		{"status": "open^x"},
		{"created_after": "2026-11-20T10:00:00Z^x"},
	} {
		if isErr, _, _ := call(ticket.ToolSearch, in); !isErr {
			t.Errorf("accepted %v", in)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("an injection reached ServiceNow: %v", m.calls)
	}
}

func TestTicketIDValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "INC0010001", sysID + "x", "../" + sysID, sysID + "?a=1", strings.Repeat("g", 32)} {
		for _, tool := range []string{ticket.ToolGet} {
			if isErr, _, _ := call(tool, map[string]any{"ticket_id": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
		if isErr, _, _ := call(ticket.ToolClose, map[string]any{"ticket_id": bad, "resolution": "x"}); !isErr {
			t.Errorf("close accepted %q", bad)
		}
		if isErr, _, _ := call(ticket.ToolComment, map[string]any{"ticket_id": bad, "body": "x"}); !isErr {
			t.Errorf("comment accepted %q", bad)
		}
		if isErr, _, _ := call(ticket.ToolUpdate, map[string]any{"ticket_id": bad, "title": "x"}); !isErr {
			t.Errorf("update accepted %q", bad)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad id reached ServiceNow: %v", m.calls)
	}
}

func TestAnnotationsAndReadOnly(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 6 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, ticket.ToolCreate, ticket.ToolClose)
	kittest.AssertReversible(t, tools, ticket.ToolUpdate, ticket.ToolUpdate)
	kittest.AssertIrreversible(t, tools, ticket.ToolComment)
	if a := tools[ticket.ToolClose].Annotations; a == nil || a.DestructiveHint == nil || *a.DestructiveHint || a.ReadOnlyHint {
		t.Errorf("close annotations = %+v", a)
	}
	if !strings.Contains(tools[ticket.ToolClose].Description, "A person can reopen") {
		t.Error("close does not say how to undo it")
	}
	for _, n := range []string{ticket.ToolGet, ticket.ToolSearch} {
		if a := tools[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is not read only", n)
		}
	}
	call(ticket.ToolGet, map[string]any{"ticket_id": sysID})
	call(ticket.ToolSearch, map[string]any{})
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
	plain := map[string]string{"SERVICENOW_INSTANCE_URL": "http://x.service-now.com", "SERVICENOW_USER": "u", "SERVICENOW_PASSWORD": "p"}
	if _, err := build(kit.NewEnvFrom(plain), log); err == nil {
		t.Error("plain http accepted")
	}
}
