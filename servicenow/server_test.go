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
	calls    []string
	queries  []url.Values
	bodies   []map[string]any
	auth     string
	state    string // the state that the incident has
	ignore   bool   // answer 200 and keep the old state, as a business rule can
	dedup    bool   // a search by correlation_id finds an incident
	failPOST bool   // the create call answers 503
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
	case r.Method == http.MethodPost && r.URL.Path == tbl && m.failPOST:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"Service Unavailable"},"status":"failure"}`))
	case r.Method == http.MethodPost && r.URL.Path == tbl:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":` + recordJSON("1", "3") + `}`))
	case r.Method == http.MethodGet && r.URL.Path == tbl && strings.Contains(r.URL.Query().Get("sysparm_query"), "correlation_id=") && !m.dedup:
		_, _ = w.Write([]byte(`{"result":[]}`))
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
	want := "state<6^assignment_group=" + group + "^short_descriptionLIKEdisk full^opened_at>=2026-11-20 08:00:00^ORDERBYDESCopened_at"
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
	if got := m.queries[2].Get("sysparm_query"); !strings.HasPrefix(got, "state>=6^") {
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
	// No tool reopens an incident, so the close has no undo.
	kittest.AssertIrreversible(t, tools, ticket.ToolClose)
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

func TestCodeTagsAreNeutralized(t *testing.T) {
	m, call, _ := setup(t)
	call(ticket.ToolCreate, map[string]any{"title": "a [code]x[/code]", "description": "b [CODE]<script>[/Code]"})
	call(ticket.ToolUpdate, map[string]any{"ticket_id": sysID, "title": "[code]t", "description": "[/code]d"})
	call(ticket.ToolComment, map[string]any{"ticket_id": sysID, "body": "n [code]<b>[/code]"})
	call(ticket.ToolClose, map[string]any{"ticket_id": sysID, "resolution": "r [Code]x"})
	var all []string
	for _, b := range m.bodies {
		for _, v := range b {
			if s, ok := v.(string); ok {
				all = append(all, s)
			}
		}
	}
	text := strings.ToLower(strings.Join(all, "\n"))
	if strings.Contains(text, "[code]") || strings.Contains(text, "[/code]") {
		t.Errorf("a code tag reached ServiceNow: %v", all)
	}
	if !strings.Contains(text, "[ code ]") || !strings.Contains(text, "[ /code ]") {
		t.Errorf("the tags were dropped and not neutralized: %v", all)
	}
	if got := plain("x [CoDe]y[/cOdE] z"); got != "x [ code ]y[ /code ] z" {
		t.Errorf("plain = %q", got)
	}
}

func TestSearchRefusesJavascript(t *testing.T) {
	m, call, _ := setup(t)
	for _, in := range []string{"javascript:gs.log(1)", "JavaScript:x", "a javascript:b", "x JAVASCRIPT:"} {
		if isErr, _, _ := call(ticket.ToolSearch, map[string]any{"title_contains": in}); !isErr {
			t.Errorf("accepted %q", in)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("reached ServiceNow: %v", m.calls)
	}
}

// open means state below 6 and closed means 6 and higher, in the search and in get_ticket.
func TestOpenMeansTheSameInSearchAndGet(t *testing.T) {
	m, call, _ := setup(t)
	for state, closed := range map[string]bool{"1": false, "2": false, "3": false, "5": false, "6": true, "7": true, "8": true} {
		m.state = state
		_, _, res := call(ticket.ToolGet, map[string]any{"ticket_id": sysID})
		var out ticket.Ticket
		kittest.Structured(t, res, &out)
		if out.Closed != closed {
			t.Errorf("state %s: closed = %v", state, out.Closed)
		}
	}
	call(ticket.ToolSearch, map[string]any{})
	call(ticket.ToolSearch, map[string]any{"status": "closed"})
	last := len(m.queries)
	if m.queries[last-2].Get("sysparm_query") != "state<6^ORDERBYDESCopened_at" || m.queries[last-1].Get("sysparm_query") != "state>=6^ORDERBYDESCopened_at" {
		t.Errorf("queries = %v %v", m.queries[last-2], m.queries[last-1])
	}
}

func TestDedupKey(t *testing.T) {
	m, call, _ := setup(t)
	args := map[string]any{"title": "a", "description": "b", "dedup_key": "finding/42:disk-full"}
	isErr, text, res := call(ticket.ToolCreate, args)
	if isErr {
		t.Fatal(text)
	}
	var out ticket.Created
	kittest.Structured(t, res, &out)
	if out.Existing {
		t.Error("a new incident is marked existing")
	}
	// The search comes first, then the create, and the key is stored in correlation_id.
	if len(m.calls) != 2 || !strings.HasPrefix(m.calls[0], "GET ") || !strings.HasPrefix(m.calls[1], "POST ") {
		t.Fatalf("calls = %v", m.calls)
	}
	if q := m.queries[0].Get("sysparm_query"); q != "correlation_id=finding/42:disk-full^ORDERBYopened_at" {
		t.Errorf("search = %q", q)
	}
	if m.bodies[1]["correlation_id"] != "finding/42:disk-full" {
		t.Errorf("body = %v", m.bodies[1])
	}
	// A retry finds the incident and makes no new one.
	m.dedup, m.calls = true, nil
	_, _, res = call(ticket.ToolCreate, args)
	var again ticket.Created
	kittest.Structured(t, res, &again)
	if !again.Existing || again.TicketID != sysID || len(m.calls) != 1 {
		t.Errorf("retry: %+v calls %v", again, m.calls)
	}
	// A key with the character ^ cannot add a condition.
	m.calls = nil
	for _, bad := range []string{"a^active=false", "a b", "a\nb", strings.Repeat("a", 101)} {
		if isErr, _, _ := call(ticket.ToolCreate, map[string]any{"title": "a", "description": "b", "dedup_key": bad}); !isErr {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad key reached ServiceNow: %v", m.calls)
	}
}

// TestCreateIsNotRepeated fails when the create call gets Retry: true. A 503
// would then make a second POST, and a second incident.
func TestCreateIsNotRepeated(t *testing.T) {
	m, call, _ := setup(t)
	m.failPOST = true
	isErr, text, _ := call(ticket.ToolCreate, map[string]any{"title": "a", "description": "b"})
	if !isErr || !strings.Contains(text, "Service Unavailable") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	posts := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST ") {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("the create call was sent %d times: %v", posts, m.calls)
	}
}

// A search by dedup_key matches correlation_id exactly, with the oldest incident
// first. The status defaults to any, so a closed incident is found.
func TestSearchByDedupKey(t *testing.T) {
	m, call, _ := setup(t)
	m.dedup = true // the incident has the key in correlation_id
	isErr, text, res := call(ticket.ToolSearch, map[string]any{"dedup_key": "finding/42:disk-full"})
	if isErr {
		t.Fatal(text)
	}
	if got := m.queries[0].Get("sysparm_query"); got != "correlation_id=finding/42:disk-full^ORDERBYopened_at" {
		t.Errorf("query = %q", got)
	}
	var out ticket.SearchOutput
	kittest.Structured(t, res, &out)
	if out.Count != len(out.Tickets) || out.Count != 1 {
		t.Errorf("out = %+v", out)
	}
	call(ticket.ToolSearch, map[string]any{"dedup_key": "k1", "status": "open", "queue": group})
	want := "state<6^assignment_group=" + group + "^correlation_id=k1^ORDERBYopened_at"
	if got := m.queries[1].Get("sysparm_query"); got != want {
		t.Errorf("query = %q", got)
	}
}

// A dedup_key cannot add a condition to the encoded query.
func TestSearchByDedupKeyRefusesInjection(t *testing.T) {
	m, call, _ := setup(t)
	for _, k := range []string{"x^active=false", "x^ORDERBYsys_id", "javascript:alert(1)", "a b", "x=y^z", strings.Repeat("a", 101)} {
		if isErr, _, _ := call(ticket.ToolSearch, map[string]any{"dedup_key": k}); !isErr {
			t.Errorf("accepted dedup_key %q", k)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("an injection reached ServiceNow: %v", m.calls)
	}
	// The query builder refuses ^ and javascript: even if the format check were absent.
	for _, k := range []string{"x^y", "JavaScript:x"} {
		if _, err := encodedQuery(ticket.SearchInput{DedupKey: k}); err == nil {
			t.Errorf("encodedQuery accepted %q", k)
		}
	}
}
