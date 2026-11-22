package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
	"github.com/zeroroot-ai/connectors/internal/ticket"
)

const key = "SEC-12"

// The mock follows the Jira Cloud REST API v3
// (https://developer.atlassian.com/cloud/jira/platform/rest/v3/): create answers
// 201 {id,key,self}, edit and transition answer 204, a comment answers 201,
// the search is POST /rest/api/3/search/jql, and an error body is
// {"errorMessages":[...],"errors":{...}}.
type mock struct {
	calls       []string
	bodies      []map[string]any
	auth        string
	transitions string // the answer of the transitions call
	failMove    bool
}

const issueJSON = `{"id":"10001","key":"SEC-12","fields":{"summary":"Disk full","status":{"name":"Done","statusCategory":{"key":"done"}},` +
	`"priority":{"name":"High"},"created":"2026-11-20T10:00:00.000+0200","updated":"2026-11-20T11:30:00.000+0000",` +
	`"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"line 1"}]},` +
	`{"type":"paragraph","content":[{"type":"text","text":"line 2"},{"type":"hardBreak"},{"type":"text","text":"line 3"}]}]}}}`

const oneDone = `{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress","statusCategory":{"key":"indeterminate"}}},` +
	`{"id":"31","name":"Done","to":{"name":"Done","statusCategory":{"key":"done"}}}]}`

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.auth = r.Header.Get("Authorization")
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	m.bodies = append(m.bodies, body)
	const issues = "/rest/api/3/issue"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == issues:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"10001","key":"SEC-12","self":"https://x/rest/api/3/issue/10001"}`))
	case r.Method == http.MethodPut && r.URL.Path == issues+"/"+key:
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == issues+"/"+key:
		_, _ = w.Write([]byte(issueJSON))
	case r.Method == http.MethodPost && r.URL.Path == issues+"/"+key+"/comment":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"1","created":"2026-11-20T10:00:00.000+0000"}`))
	case r.Method == http.MethodGet && r.URL.Path == issues+"/"+key+"/transitions":
		_, _ = w.Write([]byte(m.transitions))
	case r.Method == http.MethodPost && r.URL.Path == issues+"/"+key+"/transitions":
		if m.failMove {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errorMessages":[],"errors":{"resolution":"Resolution is required."}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/search/jql":
		_, _ = w.Write([]byte(`{"issues":[` + issueJSON + `],"isLast":true}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`))
	}
}

func setupWith(t *testing.T, extra map[string]string) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{transitions: oneDone}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	env := map[string]string{"JIRA_SITE_URL": ts.URL, "JIRA_EMAIL": "bot@example.com", "JIRA_API_TOKEN": "tok"}
	for k, v := range extra {
		env[k] = v
	}
	register, err := buildWith(kit.NewEnvFrom(env), slog.New(slog.DiscardHandler), ts.Client())
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

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	return setupWith(t, nil)
}

func TestCreateTicket(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call(ticket.ToolCreate, map[string]any{
		"title": "Disk full", "description": "line 1\n\nline 3", "queue": "SEC", "priority": "critical", "labels": []string{"zeroroot", "finding:42"},
	})
	if isErr {
		t.Fatal(text)
	}
	var out ticket.Created
	kittest.Structured(t, res, &out)
	if out.TicketID != key || out.Number != key || !strings.HasSuffix(out.URL, "/browse/SEC-12") {
		t.Errorf("out = %+v", out)
	}
	f, _ := m.bodies[0]["fields"].(map[string]any)
	if f["summary"] != "Disk full" || f["project"].(map[string]any)["key"] != "SEC" || f["issuetype"].(map[string]any)["name"] != "Task" ||
		f["priority"].(map[string]any)["name"] != "Highest" || len(f["labels"].([]any)) != 2 {
		t.Errorf("fields = %v", f)
	}
	// The description is ADF with one paragraph for each line, and an empty line is an empty paragraph.
	d := f["description"].(map[string]any)
	paras := d["content"].([]any)
	if d["type"] != "doc" || d["version"] != float64(1) || len(paras) != 3 || len(paras[1].(map[string]any)["content"].([]any)) != 0 {
		t.Errorf("description = %v", d)
	}
	if !strings.HasPrefix(m.auth, "Basic ") {
		t.Errorf("auth = %q", m.auth)
	}
}

func TestCreateRefusesBadInput(t *testing.T) {
	m, call, _ := setup(t)
	for name, in := range map[string]map[string]any{
		"no queue":   {"title": "a", "description": "b"},
		"bad queue":  {"title": "a", "description": "b", "queue": "sec"},
		"queue path": {"title": "a", "description": "b", "queue": "SEC/../X"},
		"bad label":  {"title": "a", "description": "b", "queue": "SEC", "labels": []string{"a b"}},
		"no title":   {"description": "b", "queue": "SEC"},
		"bad prio":   {"title": "a", "description": "b", "queue": "SEC", "priority": "p1"},
	} {
		if isErr, _, _ := call(ticket.ToolCreate, in); !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached Jira: %v", m.calls)
	}
}

func TestUpdateAndComment(t *testing.T) {
	m, call, _ := setup(t)
	if isErr, text, _ := call(ticket.ToolUpdate, map[string]any{"ticket_id": key, "title": "New", "priority": "low"}); isErr {
		t.Fatal(text)
	}
	f := m.bodies[0]["fields"].(map[string]any)
	if f["summary"] != "New" || f["priority"].(map[string]any)["name"] != "Low" || f["description"] != nil {
		t.Errorf("update fields = %v", f)
	}
	if isErr, text, _ := call(ticket.ToolComment, map[string]any{"ticket_id": key, "body": "checked"}); isErr {
		t.Fatal(text)
	}
	b := m.bodies[1]["body"].(map[string]any)
	if b["type"] != "doc" || m.calls[1] != "POST /rest/api/3/issue/SEC-12/comment" {
		t.Errorf("comment = %v %v", m.calls[1], b)
	}
}

func TestCloseTicket(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call(ticket.ToolClose, map[string]any{"ticket_id": key, "resolution": "patched"})
	if isErr {
		t.Fatal(text)
	}
	want := []string{
		"GET /rest/api/3/issue/SEC-12/transitions",
		"POST /rest/api/3/issue/SEC-12/comment",
		"POST /rest/api/3/issue/SEC-12/transitions",
	}
	if strings.Join(m.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v", m.calls)
	}
	if id := m.bodies[2]["transition"].(map[string]any)["id"]; id != "31" || m.bodies[2]["fields"] != nil {
		t.Errorf("transition body = %v", m.bodies[2])
	}
	var out ticket.Changed
	kittest.Structured(t, res, &out)
	if out.Status != "Done" {
		t.Errorf("out = %+v", out)
	}
}

func TestCloseSetsTheConfiguredResolution(t *testing.T) {
	m, call, _ := setupWith(t, map[string]string{"JIRA_RESOLUTION": "Done"})
	if isErr, text, _ := call(ticket.ToolClose, map[string]any{"ticket_id": key, "resolution": "x"}); isErr {
		t.Fatal(text)
	}
	if r := m.bodies[2]["fields"].(map[string]any)["resolution"].(map[string]any)["name"]; r != "Done" {
		t.Errorf("body = %v", m.bodies[2])
	}
}

func TestCloseChangesNothingWhenTheTransitionIsNotOne(t *testing.T) {
	for name, tr := range map[string]string{
		"none": `{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress","statusCategory":{"key":"indeterminate"}}}]}`,
		"two": `{"transitions":[{"id":"31","name":"Done","to":{"name":"Done","statusCategory":{"key":"done"}}},` +
			`{"id":"41","name":"Won't do","to":{"name":"Won't do","statusCategory":{"key":"done"}}}]}`,
	} {
		m, call, _ := setup(t)
		m.transitions = tr
		isErr, text, _ := call(ticket.ToolClose, map[string]any{"ticket_id": key, "resolution": "x"})
		if !isErr || !strings.Contains(text, "list_transitions") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		if len(m.calls) != 1 {
			t.Errorf("%s: the tool wrote: %v", name, m.calls)
		}
	}
}

func TestCloseShowsAFailedTransitionAfterTheComment(t *testing.T) {
	m, call, _ := setup(t)
	m.failMove = true
	isErr, text, _ := call(ticket.ToolClose, map[string]any{"ticket_id": key, "resolution": "x"})
	if !isErr || !strings.Contains(text, "Resolution is required") || !strings.Contains(text, "comment was added") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestListAndDoTransition(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("list_transitions", map[string]any{"ticket_id": key})
	if isErr {
		t.Fatal(text)
	}
	var out listTransitionsOut
	kittest.Structured(t, res, &out)
	if len(out.Transitions) != 2 || out.Transitions[1].ID != "31" || !out.Transitions[1].ToDone || out.Transitions[0].ToDone {
		t.Errorf("out = %+v", out)
	}
	if isErr, text, _ := call("transition_ticket", map[string]any{"ticket_id": key, "transition_id": "11"}); isErr {
		t.Fatal(text)
	}
	if m.bodies[1]["transition"].(map[string]any)["id"] != "11" {
		t.Errorf("body = %v", m.bodies[1])
	}
	n := len(m.calls)
	for _, bad := range []string{"", "x", "11/../1", "1;2", strings.Repeat("1", 10)} {
		if isErr, _, _ := call("transition_ticket", map[string]any{"ticket_id": key, "transition_id": bad}); !isErr {
			t.Errorf("accepted transition %q", bad)
		}
	}
	if len(m.calls) != n {
		t.Errorf("a bad transition reached Jira: %v", m.calls[n:])
	}
}

func TestGetTicket(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call(ticket.ToolGet, map[string]any{"ticket_id": key})
	if isErr {
		t.Fatal(text)
	}
	var out ticket.Ticket
	kittest.Structured(t, res, &out)
	if out.TicketID != key || out.Title != "Disk full" || out.Status != "Done" || !out.Closed || out.Priority != ticket.High ||
		out.Description != "line 1\nline 2\nline 3" || out.CreatedAt != "2026-11-20T08:00:00Z" || out.UpdatedAt != "2026-11-20T11:30:00Z" {
		t.Errorf("out = %+v", out)
	}
	if m.calls[0] != "GET /rest/api/3/issue/SEC-12" {
		t.Errorf("calls = %v", m.calls)
	}
	if isErr, text, _ := call(ticket.ToolGet, map[string]any{"ticket_id": "SEC-99"}); !isErr || !strings.Contains(text, "does not exist") {
		t.Errorf("missing issue: isErr=%v %s", isErr, text)
	}
}

func TestSearchBuildsTheJQLFromTypedFilters(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call(ticket.ToolSearch, map[string]any{
		"queue": "SEC", "title_contains": "disk full", "label": "zeroroot", "created_after": "2026-11-20T10:00:00+02:00", "limit": 5,
	})
	if isErr {
		t.Fatal(text)
	}
	want := `statusCategory != Done AND project = "SEC" AND summary ~ "disk full" AND labels = "zeroroot" AND created >= "2026-11-20 08:00" ORDER BY created DESC`
	if got := m.bodies[0]["jql"]; got != want || m.bodies[0]["maxResults"] != float64(5) {
		t.Errorf("jql = %v", got)
	}
	var out ticket.SearchOutput
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Tickets[0].TicketID != key {
		t.Errorf("out = %+v", out)
	}
	if isErr, _, _ := call(ticket.ToolSearch, map[string]any{"status": "closed"}); isErr || m.bodies[1]["jql"] != "statusCategory = Done ORDER BY created DESC" {
		t.Errorf("closed: %v", m.bodies[1]["jql"])
	}
	if isErr, _, _ := call(ticket.ToolSearch, map[string]any{"status": "any"}); !isErr {
		t.Error("an unbounded search was accepted")
	}
}

var stringLiteral = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// TestSearchCannotInjectJQL feeds hostile texts and makes sure that each one
// stays inside one string literal: with the literals removed, the query is the
// fixed skeleton of the filters.
func TestSearchCannotInjectJQL(t *testing.T) {
	hostile := []string{
		`x" OR project = "SEC`, `x" ORDER BY created ASC --`, `\" OR 1=1 --`, `a\`, `a\\" AND "b`, `")) OR (summary ~ "`,
		`x" AND assignee = currentUser() AND "`, `"`, `\`, `"" ""`, "x y",
	}
	skeleton := "statusCategory != Done AND summary ~  AND labels =  ORDER BY created DESC"
	for _, h := range hostile {
		if strings.ContainsAny(h, "\n\r\t") {
			continue
		}
		jql, err := buildJQL(ticket.SearchInput{TitleContains: h, Label: "ok"})
		if err != nil {
			t.Errorf("%q: %v", h, err)
			continue
		}
		if rest := stringLiteral.ReplaceAllString(jql, ""); rest != skeleton {
			t.Errorf("%q escapes its literal: jql %s, rest %q", h, jql, rest)
		}
	}
	// A label and a queue are checked by a pattern, so they never hold a quote.
	m, call, _ := setup(t)
	for _, in := range []map[string]any{{"label": `x" OR "a`}, {"queue": `SEC" OR project = "X`}, {"queue": "SEC OR X"}, {"title_contains": "a\nb"}, {"created_after": `2026-11-20T10:00:00Z" OR "`}} {
		if isErr, _, _ := call(ticket.ToolSearch, in); !isErr {
			t.Errorf("accepted %v", in)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("an injection reached Jira: %v", m.calls)
	}
}

func TestTicketIDValidation(t *testing.T) {
	m, call, _ := setup(t)
	for _, bad := range []string{"", "sec-12", "SEC-0", "SEC-12/comment", "../SEC-12", "SEC-12?x=1", "10001", "SEC-12 ", strings.Repeat("A", 30) + "-1"} {
		for _, tool := range []string{ticket.ToolGet, "list_transitions"} {
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
		t.Errorf("a bad id reached Jira: %v", m.calls)
	}
}

func TestAnnotationsAndReadOnly(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 8 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, ticket.ToolCreate, ticket.ToolClose)
	kittest.AssertReversible(t, tools, ticket.ToolUpdate, ticket.ToolUpdate)
	kittest.AssertReversible(t, tools, "transition_ticket", "transition_ticket")
	kittest.AssertIrreversible(t, tools, ticket.ToolComment)
	if !strings.Contains(tools[ticket.ToolClose].Description, "A person can reopen") {
		t.Error("close does not say how to undo it")
	}
	for _, n := range []string{ticket.ToolGet, ticket.ToolSearch, "list_transitions"} {
		if a := tools[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is not read only", n)
		}
	}
	call(ticket.ToolGet, map[string]any{"ticket_id": key})
	call("list_transitions", map[string]any{"ticket_id": key})
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
	base := map[string]string{"JIRA_SITE_URL": "https://x.atlassian.net", "JIRA_EMAIL": "a@b.c", "JIRA_API_TOKEN": "t"}
	for name, extra := range map[string]map[string]string{
		"plain http":     {"JIRA_SITE_URL": "http://x.atlassian.net"},
		"bad issue type": {"JIRA_ISSUE_TYPE": `Task"}`},
		"bad resolution": {"JIRA_RESOLUTION": "a;b"},
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
	if _, err := build(kit.NewEnvFrom(base), log); err != nil {
		t.Errorf("good config: %v", err)
	}
}
