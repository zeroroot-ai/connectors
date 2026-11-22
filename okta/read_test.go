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

// readMock follows the Okta Management API (OpenAPI spec
// https://github.com/okta/okta-management-openapi-spec):
// GET /api/v1/logs answers a JSON array of LogEvent,
// GET /api/v1/users answers a JSON array of User,
// GET /api/v1/users/{userId} answers one User,
// An unknown id gets 404 with the error code E0000007.
type readMock struct {
	methods []string
	queries []string
	auth    string
	link    []string // Link headers of the log answer
}

const (
	sessID = "102abcDEFghiJKLmnoPQRstuVWX"
	logsJS = `[{"uuid":"e1","published":"2026-11-20T10:00:00.000Z","eventType":"user.session.start",` +
		`"displayMessage":"User login to Okta","severity":"INFO","outcome":{"result":"SUCCESS"},` +
		`"actor":{"id":"00ub0oNGTSWTBKOLGLNR","type":"User","alternateId":"a@example.com","displayName":"A B"},` +
		`"client":{"ipAddress":"203.0.113.5","userAgent":{"rawUserAgent":"curl/8"}},` +
		`"authenticationContext":{"externalSessionId":"` + sessID + `"},"transaction":{"id":"t1"},` +
		`"target":[{"id":"x","type":"AppInstance","alternateId":"app","displayName":"App"}],` +
		`"securityContext":{"asNumber":1},"request":{"ipChain":[]}}]`
	userJS = `{"id":"00ub0oNGTSWTBKOLGLNR","status":"ACTIVE","created":"2026-01-01T00:00:00.000Z",` +
		`"lastLogin":"2026-11-20T10:00:00.000Z","passwordChanged":"2026-02-01T00:00:00.000Z",` +
		`"credentials":{"password":{}},"profile":{"login":"a@example.com","email":"a@example.com","firstName":"A","lastName":"B","mobilePhone":"555"},"_links":{}}`
)

func (m *readMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.methods = append(m.methods, r.Method)
	m.queries = append(m.queries, r.URL.RequestURI())
	m.auth = r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/logs":
		for _, l := range m.link {
			w.Header().Add("Link", l)
		}
		_, _ = w.Write([]byte(logsJS))
	case "/api/v1/users":
		_, _ = w.Write([]byte("[" + userJS + "]"))
	case "/api/v1/users/00ub0oNGTSWTBKOLGLNR":
		_, _ = w.Write([]byte(userJS))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorCode":"E0000007","errorSummary":"Not found: Resource not found","errorId":"e1"}`))
	}
}

func readSetup(t *testing.T) (srv *readMock, call func(string, map[string]any) (bool, string, *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &readMock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{"OKTA_ORG_URL": ts.URL, "OKTA_API_TOKEN": "tok"}),
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

func TestListLogEvents(t *testing.T) {
	m, call, _ := readSetup(t)
	isErr, text, res := call("list_log_events", map[string]any{
		"since": "2026-11-20T00:00:00Z", "until": "2026-11-21T00:00:00Z", "q": "alice bob",
		"filter": `eventType eq "user.session.start"`, "limit": 10, "sort_order": "ASCENDING",
	})
	if isErr {
		t.Fatal(text)
	}
	var out logOutput
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Events[0].EventType != "user.session.start" || out.Events[0].Actor.AlternateID != "a@example.com" ||
		out.Events[0].AuthenticationContext.ExternalSessionID != sessID || out.Events[0].Client.IPAddress != "203.0.113.5" {
		t.Errorf("out = %+v", out)
	}
	for _, want := range []string{"limit=10", "sortOrder=ASCENDING", "since=2026-11-20T00%3A00%3A00Z",
		"until=2026-11-21T00%3A00%3A00Z", "q=alice+bob", "filter=eventType+eq"} {
		if !strings.Contains(m.queries[0], want) {
			t.Errorf("query %q lacks %q", m.queries[0], want)
		}
	}
	if m.auth != "SSWS tok" {
		t.Errorf("auth = %q", m.auth)
	}
	if isErr, text, _ := call("list_log_events", map[string]any{}); isErr {
		t.Fatal(text)
	}
	// The defaults: 50 events, newest first.
	if !strings.Contains(m.queries[1], "limit=50") || !strings.Contains(m.queries[1], "sortOrder=DESCENDING") {
		t.Errorf("defaults missing: %s", m.queries[1])
	}
}

// Okta sends the self link and the next link in two Link headers. The tool
// reads the after cursor of the next link and never follows the link.
func TestListLogEventsPaging(t *testing.T) {
	m, call, _ := readSetup(t)
	m.link = []string{
		`<https://evil.example/api/v1/logs?limit=50>; rel="self"`,
		`<https://evil.example/api/v1/logs?after=abc123%3A0&limit=50>; rel="next"`,
	}
	isErr, text, res := call("list_log_events", map[string]any{})
	if isErr {
		t.Fatal(text)
	}
	var out logOutput
	kittest.Structured(t, res, &out)
	if !out.Truncated || out.After != "abc123:0" {
		t.Errorf("out = truncated %v after %q", out.Truncated, out.After)
	}
	if isErr, text, _ := call("list_log_events", map[string]any{"after": out.After}); isErr {
		t.Fatal(text)
	}
	if !strings.Contains(m.queries[1], "after=abc123%3A0") {
		t.Errorf("the cursor did not reach Okta: %s", m.queries[1])
	}
	// With no next link, the answer is complete.
	m.link = []string{`<https://org/api/v1/logs?limit=50>; rel="self"`}
	_, _, res = call("list_log_events", map[string]any{})
	var done logOutput
	kittest.Structured(t, res, &done)
	if done.Truncated || done.After != "" {
		t.Errorf("a complete answer is marked truncated: %+v", done)
	}
	for _, c := range m.queries {
		if strings.Contains(c, "evil.example") {
			t.Errorf("the tool used the host of a link: %s", c)
		}
	}
}

func TestListLogEventsKeywordLimits(t *testing.T) {
	m, call, _ := readSetup(t)
	ok := strings.TrimSpace(strings.Repeat("k ", 10))
	if isErr, text, _ := call("list_log_events", map[string]any{"q": ok}); isErr {
		t.Errorf("10 keywords refused: %s", text)
	}
	n := len(m.queries)
	for name, q := range map[string]string{
		"11 keywords":  ok + " k",
		"long keyword": strings.Repeat("a", 41),
		"control char": "a\tb",
	} {
		if isErr, _, _ := call("list_log_events", map[string]any{"q": q}); !isErr {
			t.Errorf("%s accepted", name)
		}
	}
	if isErr, _, _ := call("list_log_events", map[string]any{"q": strings.Repeat("a", 40)}); isErr {
		t.Error("a keyword of 40 characters refused")
	}
	if len(m.queries) != n+1 {
		t.Errorf("a bad keyword reached Okta: %v", m.queries)
	}
}

func TestListLogEventsRefusesBadInput(t *testing.T) {
	m, call, _ := readSetup(t)
	for _, in := range []map[string]any{
		{"since": "yesterday"}, {"until": "2026-11-20"}, {"limit": 201}, {"limit": -1},
		{"sort_order": "UP"}, {"after": "a\nb"}, {"filter": strings.Repeat("a", 401)}, {"q": "a\nb"},
	} {
		if isErr, _, _ := call("list_log_events", in); !isErr {
			t.Errorf("accepted %v", in)
		}
	}
	if len(m.queries) != 0 {
		t.Errorf("a bad input reached Okta: %v", m.queries)
	}
}

func TestListUsersAndGetUser(t *testing.T) {
	m, call, _ := readSetup(t)
	isErr, text, res := call("list_users", map[string]any{"search": `profile.login eq "a@example.com"`, "q": "ali"})
	if isErr {
		t.Fatal(text)
	}
	var out usersOutput
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Users[0].Profile.Login != "a@example.com" || out.Users[0].Status != "ACTIVE" {
		t.Errorf("out = %+v", out)
	}
	if strings.Contains(text, "mobilePhone") || strings.Contains(text, "555") {
		t.Errorf("the output holds a profile field that the tool does not need: %s", text)
	}
	if !strings.Contains(m.queries[0], "search=profile.login") || !strings.Contains(m.queries[0], "limit=50") ||
		!strings.Contains(m.queries[0], "q=ali") {
		t.Errorf("query = %s", m.queries[0])
	}
	var u userInfo
	_, _, res = call("get_user", map[string]any{"user_id": uid})
	kittest.Structured(t, res, &u)
	if u.ID != uid || u.LastLogin == "" {
		t.Errorf("user = %+v", u)
	}
	if isErr, text, _ := call("get_user", map[string]any{"user_id": "00uZZZZZZZZZZZZZZZZZ"}); !isErr || !strings.Contains(text, "E0000007") {
		t.Errorf("unknown user: %v %s", isErr, text)
	}
}

func TestListUsersRefusesBadInput(t *testing.T) {
	m, call, _ := readSetup(t)
	for _, in := range []map[string]any{{}, {"q": "a", "limit": 51}, {"search": strings.Repeat("a", 401)}, {"q": "a\tb"}} {
		if isErr, _, _ := call("list_users", in); !isErr {
			t.Errorf("accepted %v", in)
		}
	}
	for _, bad := range []string{"", "a@b.com", uid + "x", "../x"} {
		if isErr, _, _ := call("get_user", map[string]any{"user_id": bad}); !isErr {
			t.Errorf("get_user accepted %q", bad)
		}
	}
	if len(m.queries) != 0 {
		t.Errorf("a bad input reached Okta: %v", m.queries)
	}
}

// TestReadToolsOnlyRead makes sure each read tool is marked read only and
// sends only GET.
func TestReadToolsOnlyRead(t *testing.T) {
	m, call, tools := readSetup(t)
	reads := map[string]map[string]any{
		"list_log_events": {},
		"list_users":      {"q": "a"},
		"get_user":        {"user_id": uid},
	}
	for name, args := range reads {
		tl := tools[name]
		if tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint || !tl.Annotations.IdempotentHint {
			t.Errorf("%s is not marked read only", name)
		}
		if isErr, text, _ := call(name, args); isErr {
			t.Errorf("%s: %s", name, text)
		}
	}
	if len(m.methods) != len(reads) {
		t.Fatalf("calls = %v", m.methods)
	}
	for _, method := range m.methods {
		if method != http.MethodGet {
			t.Errorf("a read tool sent %s", method)
		}
	}
	// The response tools stay marked as changes.
	for _, name := range []string{"revoke_user_sessions", "suspend_user", "unsuspend_user"} {
		if tools[name].Annotations.ReadOnlyHint {
			t.Errorf("%s is marked read only", name)
		}
	}
}
