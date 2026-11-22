package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/ticket"
)

// The tools use the Jira Cloud REST API v3. The reference is
// https://developer.atlassian.com/cloud/jira/platform/rest/v3/ and the OpenAPI
// spec https://dac-static.atlassian.com/cloud/jira/platform/swagger-v3.v3.json.
//   - POST /rest/api/3/issue                         (create issue, 201, {id,key,self})
//   - PUT  /rest/api/3/issue/{issueIdOrKey}          (edit issue, 204)
//   - GET  /rest/api/3/issue/{issueIdOrKey}          (get issue, query fields)
//   - POST /rest/api/3/issue/{issueIdOrKey}/comment  (add comment, 201, body is ADF)
//   - GET  /rest/api/3/issue/{issueIdOrKey}/transitions (get transitions)
//   - POST /rest/api/3/issue/{issueIdOrKey}/transitions (do transition, 204)
//   - POST /rest/api/3/search/jql                    (search with JQL: jql, fields, maxResults;
//     the old /rest/api/3/search is removed)
// Authentication: HTTP basic with the e-mail address and an API token
// (https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/).
// The description and the comment are in the Atlassian Document Format (ADF).
//
// The mapping of internal/ticket to Jira:
//   - ticket_id is the issue key. number is the same key. queue is the project key.
//   - priority: low is Low, medium is Medium, high is High, critical is Highest.
//   - closed is true when the status category of the issue is done.
//   - close_ticket adds the resolution as a comment and then does the one
//     transition whose target status is in the category done.

var (
	projectKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,9}$`)
	issueKey   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,9}-[1-9]\d{0,8}$`)
	transID    = regexp.MustCompile(`^\d{1,9}$`)
	nameFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,49}$`)
)

const (
	issueFields = "summary,description,status,priority,created,updated"
	doneKey     = "done" // the key of the status category that ends an issue
)

var priorityNames = map[string]string{
	ticket.Low: "Low", ticket.Medium: "Medium", ticket.High: "High", ticket.Critical: "Highest",
}

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("JIRA_SITE_URL")
	email := env.Require("JIRA_EMAIL")
	token := env.Secret("JIRA_API_TOKEN")
	issueType := env.Optional("JIRA_ISSUE_TYPE", "Task")
	resolution := env.Optional("JIRA_RESOLUTION", "")
	timeout := env.Duration("JIRA_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("JIRA_SITE_URL must use https")
	}
	if !nameFormat.MatchString(issueType) {
		return nil, errors.New("JIRA_ISSUE_TYPE has the wrong format")
	}
	if resolution != "" && !nameFormat.MatchString(resolution) {
		return nil, errors.New("JIRA_RESOLUTION has the wrong format")
	}
	client := kit.NewClient(base, kit.ClientOptions{
		Auth: kit.BasicAuth(email, token), Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	svc := &service{client: client, now: clock, web: *base, issueType: issueType, resolution: resolution}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

// clock is the time source of the search. Tests replace it.
var clock = time.Now

type service struct {
	client     *kit.Client
	now        func() time.Time
	web        url.URL
	issueType  string
	resolution string // the resolution that close_ticket sets. Empty sets none.
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolCreate,
		Description: "Open one Jira issue. Give title, description and queue, the key of the project. " +
			"priority is low, medium, high or critical. The issue type is set by the server. " +
			"dedup_key makes a repeat safe: a second call with the same key returns the first issue with existing true. " +
			"No tool deletes the issue. Undo: call close_ticket with the ticket_id of the result.",
		Annotations: kit.Reversible("Create Jira issue", false),
	}, v.create)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolUpdate,
		Description: "Change the title, the description or the priority of one Jira issue. Give at least one. " +
			"Undo: call update_ticket with the old value.",
		Annotations: kit.Reversible("Update Jira issue", true),
	}, v.update)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        ticket.ToolComment,
		Description: "Add a comment to one Jira issue. No tool undoes this action: Jira keeps the comment, and only a person can delete it.",
		Annotations: kit.Irreversible("Add Jira comment", false),
	}, v.comment)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolClose,
		Description: "Close one Jira issue. The tool does the one transition of the issue that leads to a status in the category done, " +
			"and adds the resolution as a comment in the same request. When the issue has no such transition or more than one, the tool " +
			"changes nothing and names them: call list_transitions and transition_ticket. " +
			"A person can reopen the issue. Undo with a transition back, with list_transitions and transition_ticket, when the workflow has one.",
		Annotations: kit.Reversible("Close Jira issue", true),
	}, v.close)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        ticket.ToolGet,
		Description: "Read one Jira issue. Read only. status is the name of the status. closed is true when its category is done.",
		Annotations: kit.ReadOnly("Get Jira issue"),
	}, v.get)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolSearch,
		Description: "Search Jira issues with typed filters: queue (project key), status (open, closed or any), title_contains, label, " +
			"created_after and limit. Newest first. Read only. The tool builds the JQL from the filters. A text is quoted and escaped, " +
			"so it cannot add a clause. Give at least one filter or a status of open or closed.",
		Annotations: kit.ReadOnly("Search Jira issues"),
	}, v.search)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "list_transitions",
		Description: "List the transitions that the current user can do on one Jira issue: id, name and the target status with its category. Read only.",
		Annotations: kit.ReadOnly("List Jira transitions"),
	}, v.listTransitions)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "transition_ticket",
		Description: "Do one transition on one Jira issue, with an id from list_transitions. " +
			"Undo: call list_transitions and then transition_ticket with a transition back, when the workflow has one.",
		Annotations: kit.Reversible("Transition Jira issue", false),
	}, v.transition)
}

// ---- ADF ---------------------------------------------------------------

// adf returns a document of the Atlassian Document Format with one paragraph
// for each line of the text.
func adf(text string) map[string]any {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	paras := make([]any, 0, len(lines))
	for _, line := range lines {
		p := map[string]any{"type": "paragraph", "content": []any{}}
		if line != "" {
			p["content"] = []any{map[string]any{"type": "text", "text": line}}
		}
		paras = append(paras, p)
	}
	return map[string]any{"type": "doc", "version": 1, "content": paras}
}

type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
}

// text reads the plain text of an ADF document. A block ends with a new line.
func (n adfNode) text() string {
	switch n.Type {
	case "text":
		return n.Text
	case "hardBreak":
		return "\n"
	}
	var b strings.Builder
	for _, c := range n.Content {
		b.WriteString(c.text())
	}
	switch n.Type {
	case "paragraph", "heading", "listItem", "codeBlock", "blockquote":
		b.WriteString("\n")
	}
	return b.String()
}

// ---- helpers -----------------------------------------------------------

func checkKey(field, key string) error {
	if err := kit.CheckString(field, key, 20, issueKey); err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	return nil
}

func issuePath(key string, more ...string) string {
	return kit.Path(append([]string{"rest", "api", "3", "issue", key}, more...)...)
}

func (v *service) browseURL(key string) string {
	u := v.web
	u.Path = strings.TrimRight(u.Path, "/") + "/browse/" + key
	return u.String()
}

// jiraError shows the messages of Jira when the answer has them. The body of
// an error is {"errorMessages": [...], "errors": {"field": "message"}}.
func jiraError(op string, err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var body struct {
			Messages []string          `json:"errorMessages"`
			Errors   map[string]string `json:"errors"`
		}
		if kit.DecodeErrorBody(ae, &body) == nil && (len(body.Messages) > 0 || len(body.Errors) > 0) {
			parts := append([]string{}, body.Messages...)
			for k, m := range body.Errors {
				parts = append(parts, k+": "+m)
			}
			return fmt.Errorf("%s: Jira answered HTTP %d: %s", op, ae.Status, strings.Join(parts, "; "))
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ---- tools -------------------------------------------------------------

func (v *service) create(ctx context.Context, _ *mcp.CallToolRequest, in ticket.CreateInput) (*mcp.CallToolResult, ticket.Created, error) {
	var out ticket.Created
	if err := ticket.CheckCreate(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	if !projectKey.MatchString(in.Queue) {
		return nil, out, fmt.Errorf("%w: queue must be the key of a Jira project, for example SEC", kit.ErrInvalidInput)
	}
	prio := in.Priority
	if prio == "" {
		prio = ticket.Medium
	}
	f := map[string]any{
		"project": map[string]any{"key": in.Queue}, "summary": in.Title, "issuetype": map[string]any{"name": v.issueType},
		"description": adf(in.Description), "priority": map[string]any{"name": priorityNames[prio]},
	}
	labels := append([]string{}, in.Labels...)
	if in.DedupKey != "" {
		existing, err := v.byDedupKey(ctx, in.DedupKey)
		if err != nil {
			return nil, out, err
		}
		if existing != "" {
			return nil, ticket.Created{TicketID: existing, Number: existing, URL: v.browseURL(existing), Existing: true}, nil
		}
		labels = append(labels, dedupLabel(in.DedupKey))
	}
	if len(labels) > 0 {
		f["labels"] = labels
	}
	var resp struct {
		Key string `json:"key"`
	}
	// The client does not repeat a create call: a repeat after a timeout would
	// open a second issue. The caller repeats it with the same dedup_key.
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("rest", "api", "3", "issue"), JSON: map[string]any{"fields": f},
	}, &resp)
	if err != nil {
		return nil, out, jiraError("create issue", err)
	}
	if !issueKey.MatchString(resp.Key) {
		return nil, out, errors.New("create issue: Jira gave no issue key in the answer")
	}
	return nil, ticket.Created{TicketID: resp.Key, Number: resp.Key, URL: v.browseURL(resp.Key)}, nil
}

func (v *service) update(ctx context.Context, _ *mcp.CallToolRequest, in ticket.UpdateInput) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: ticket.ToolUpdate}
	if err := checkKey("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := ticket.CheckUpdate(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	f := map[string]any{}
	if in.Title != "" {
		f["summary"] = in.Title
	}
	if in.Description != "" {
		f["description"] = adf(in.Description)
	}
	if in.Priority != "" {
		f["priority"] = map[string]any{"name": priorityNames[in.Priority]}
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPut, Path: issuePath(in.TicketID), JSON: map[string]any{"fields": f},
	}, nil)
	if err != nil {
		return nil, out, jiraError("update issue", err)
	}
	return nil, out, nil
}

func (v *service) addComment(ctx context.Context, key, text string) error {
	// A comment is added by each request, so the call is not repeated.
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: issuePath(key, "comment"), JSON: map[string]any{"body": adf(text)},
	}, nil)
	if err != nil {
		return jiraError("add comment", err)
	}
	return nil
}

func (v *service) comment(ctx context.Context, _ *mcp.CallToolRequest, in ticket.CommentInput) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: ticket.ToolComment}
	if err := checkKey("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := ticket.CheckText("body", in.Body, ticket.MaxComment); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	if err := v.addComment(ctx, in.TicketID, in.Body); err != nil {
		return nil, out, err
	}
	return nil, out, nil
}

type transition struct {
	ID string `json:"id"`
	To struct {
		Name           string `json:"name"`
		StatusCategory struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	} `json:"to"`
	Name string `json:"name"`
}

func (v *service) transitions(ctx context.Context, key string) ([]transition, error) {
	var resp struct {
		Transitions []transition `json:"transitions"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: issuePath(key, "transitions")}, &resp); err != nil {
		return nil, jiraError("list transitions", err)
	}
	for _, t := range resp.Transitions {
		if !transID.MatchString(t.ID) {
			return nil, errors.New("list transitions: Jira gave a transition with no numeric id")
		}
	}
	return resp.Transitions, nil
}

func (v *service) doTransition(ctx context.Context, key, id string) error {
	body := map[string]any{"transition": map[string]any{"id": id}}
	err := v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: issuePath(key, "transitions"), JSON: body}, nil)
	if err != nil {
		return jiraError("transition issue", err)
	}
	return nil
}

func (v *service) close(ctx context.Context, _ *mcp.CallToolRequest, in ticket.CloseInput) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: ticket.ToolClose}
	if err := checkKey("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := ticket.CheckText("resolution", in.Resolution, ticket.MaxComment); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	all, err := v.transitions(ctx, in.TicketID)
	if err != nil {
		return nil, out, err
	}
	var done []transition
	for _, t := range all {
		if t.To.StatusCategory.Key == doneKey {
			done = append(done, t)
		}
	}
	if len(done) != 1 {
		names := make([]string, 0, len(done))
		for _, t := range done {
			names = append(names, t.ID+" "+t.Name)
		}
		return nil, out, fmt.Errorf("%w: the issue has %d transitions to a done status (%s). Nothing changed. "+
			"Call list_transitions and transition_ticket", kit.ErrInvalidInput, len(done), strings.Join(names, ", "))
	}
	// The resolution goes in the same request as the transition. Jira does both
	// or neither, so no issue is left with a comment and no close.
	body := map[string]any{
		"transition": map[string]any{"id": done[0].ID},
		"update":     map[string]any{"comment": []any{map[string]any{"add": map[string]any{"body": adf(in.Resolution)}}}},
	}
	if v.resolution != "" {
		body["fields"] = map[string]any{"resolution": map[string]any{"name": v.resolution}}
	}
	err = v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: issuePath(in.TicketID, "transitions"), JSON: body}, nil)
	if err != nil {
		return nil, out, jiraError("close issue", err)
	}
	out.Status = done[0].To.Name
	return nil, out, nil
}

type transitionOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ToStatus string `json:"to_status"`
	ToDone   bool   `json:"to_done"`
}

type listTransitionsOut struct {
	Transitions []transitionOut `json:"transitions"`
}

func (v *service) listTransitions(ctx context.Context, _ *mcp.CallToolRequest, in ticket.GetInput) (*mcp.CallToolResult, listTransitionsOut, error) {
	out := listTransitionsOut{Transitions: []transitionOut{}}
	if err := checkKey("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	all, err := v.transitions(ctx, in.TicketID)
	if err != nil {
		return nil, out, err
	}
	for _, t := range all {
		out.Transitions = append(out.Transitions, transitionOut{ID: t.ID, Name: t.Name, ToStatus: t.To.Name, ToDone: t.To.StatusCategory.Key == doneKey})
	}
	return nil, out, nil
}

type transitionIn struct {
	TicketID     string `json:"ticket_id"`
	TransitionID string `json:"transition_id" jsonschema:"the id from list_transitions: digits"`
}

func (v *service) transition(ctx context.Context, _ *mcp.CallToolRequest, in transitionIn) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: "transition_ticket"}
	if err := checkKey("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := kit.CheckString("transition_id", in.TransitionID, 9, transID); err != nil {
		return nil, out, fmt.Errorf("check transition_id: %w", err)
	}
	if err := v.doTransition(ctx, in.TicketID, in.TransitionID); err != nil {
		return nil, out, err
	}
	return nil, out, nil
}

type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string  `json:"summary"`
		Description adfNode `json:"description"`
		Created     string  `json:"created"`
		Updated     string  `json:"updated"`
		Status      struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
	} `json:"fields"`
}

func priorityOf(name string) string {
	switch name {
	case "Highest", "Blocker", "Critical":
		return ticket.Critical
	case "High", "Major":
		return ticket.High
	case "Medium":
		return ticket.Medium
	case "Low", "Lowest", "Minor", "Trivial":
		return ticket.Low
	}
	return ""
}

func (v *service) toTicket(i issue) ticket.Ticket {
	return ticket.Ticket{
		TicketID: i.Key, Number: i.Key, URL: v.browseURL(i.Key), Title: i.Fields.Summary,
		Description: strings.TrimRight(i.Fields.Description.text(), "\n"), Status: i.Fields.Status.Name,
		Priority: priorityOf(i.Fields.Priority.Name), Closed: i.Fields.Status.StatusCategory.Key == doneKey,
		CreatedAt: jiraTime(i.Fields.Created), UpdatedAt: jiraTime(i.Fields.Updated),
	}
}

// jiraTime reads a time of Jira ("2026-11-20T10:00:00.000+0200") as RFC 3339
// in UTC. A value that does not parse is returned as it is.
func jiraTime(s string) string {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

func (v *service) get(ctx context.Context, _ *mcp.CallToolRequest, in ticket.GetInput) (*mcp.CallToolResult, ticket.Ticket, error) {
	var out ticket.Ticket
	if err := checkKey("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	var i issue
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: issuePath(in.TicketID), Query: url.Values{"fields": {issueFields}}}, &i)
	if err != nil {
		return nil, out, jiraError("get issue", err)
	}
	if i.Key != in.TicketID {
		return nil, out, errors.New("get issue: Jira did not return the issue")
	}
	return nil, v.toTicket(i), nil
}

// jqlString quotes a text as a JQL string. Inside the quotes, a backslash and
// a double quote have an escape. So the text cannot end the string and add a
// clause. A control character is refused before this call.
func jqlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// luceneSpecial holds the characters that the text search of Jira (Lucene)
// reads as syntax: + - & | ! ( ) { } [ ] ^ " ~ * ? \\ :
const luceneSpecial = `+-&|!(){}[]^"~*?\\:`

// luceneEscape puts a backslash before each character of the text search syntax,
// so the text matches itself and not a wildcard, a range or an operator.
// jqlString then doubles each backslash, which gives the form \\+ that the
// Atlassian pages show.
func luceneEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(luceneSpecial, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// buildJQL builds the query from the typed filters.
func buildJQL(in ticket.SearchInput, now time.Time) (string, error) {
	var conds []string
	switch ticket.StatusOf(in) {
	case ticket.StatusOpen:
		conds = append(conds, "statusCategory != Done")
	case ticket.StatusClosed:
		conds = append(conds, "statusCategory = Done")
	}
	if in.Queue != "" {
		if !projectKey.MatchString(in.Queue) {
			return "", fmt.Errorf("%w: queue must be the key of a Jira project, for example SEC", kit.ErrInvalidInput)
		}
		conds = append(conds, "project = "+jqlString(in.Queue))
	}
	if in.TitleContains != "" {
		conds = append(conds, "summary ~ "+jqlString(luceneEscape(in.TitleContains)))
	}
	if in.Label != "" {
		conds = append(conds, "labels = "+jqlString(in.Label))
	}
	if in.CreatedAfter != "" {
		t, err := time.Parse(time.RFC3339, in.CreatedAfter)
		if err != nil {
			return "", fmt.Errorf("%w: created_after is not an RFC 3339 time", kit.ErrInvalidInput)
		}
		// A date in JQL is read in the time zone of the user of the token, and
		// the tool does not know that zone. A relative time is counted by Jira
		// from its own clock, so the tool sends the minutes since the time.
		if t.After(now) {
			return "", fmt.Errorf("%w: created_after is in the future", kit.ErrInvalidInput)
		}
		minutes := int(now.Sub(t)/time.Minute) + 1 // round up, so the issue at the time is in
		conds = append(conds, "created >= -"+strconv.Itoa(minutes)+"m")
	}
	if len(conds) == 0 {
		return "", fmt.Errorf("%w: give at least one filter. Jira refuses an unbounded search", kit.ErrInvalidInput)
	}
	return strings.Join(conds, " AND ") + " ORDER BY created DESC", nil
}

func (v *service) search(ctx context.Context, _ *mcp.CallToolRequest, in ticket.SearchInput) (*mcp.CallToolResult, ticket.SearchOutput, error) {
	out := ticket.SearchOutput{Tickets: []ticket.Ticket{}}
	limit, err := ticket.CheckSearch(in)
	if err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	jql, err := buildJQL(in, v.now())
	if err != nil {
		return nil, out, err
	}
	var resp struct {
		Issues []issue `json:"issues"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("rest", "api", "3", "search", "jql"), Retry: true, // a search changes nothing
		JSON: map[string]any{"jql": jql, "fields": strings.Split(issueFields, ","), "maxResults": limit},
	}, &resp)
	if err != nil {
		return nil, out, jiraError("search issues", err)
	}
	for _, i := range resp.Issues {
		out.Tickets = append(out.Tickets, v.toTicket(i))
	}
	out.Count = len(out.Tickets)
	return nil, out, nil
}

// dedupLabel is the label that holds the dedup key of an issue.
func dedupLabel(key string) string { return "zeroroot-dedup-" + key }

// byDedupKey returns the key of the oldest issue that has the label of the
// dedup key, or an empty string. The key has a fixed pattern, and jqlString
// quotes the label, so it cannot add a clause.
func (v *service) byDedupKey(ctx context.Context, key string) (string, error) {
	var resp struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("rest", "api", "3", "search", "jql"), Retry: true, // a search changes nothing
		JSON: map[string]any{"jql": "labels = " + jqlString(dedupLabel(key)) + " ORDER BY created ASC", "fields": []string{"summary"}, "maxResults": 1},
	}, &resp)
	if err != nil {
		return "", jiraError("look for the dedup key", err)
	}
	if len(resp.Issues) == 0 {
		return "", nil
	}
	if !issueKey.MatchString(resp.Issues[0].Key) {
		return "", errors.New("look for the dedup key: Jira gave an issue with no key")
	}
	return resp.Issues[0].Key, nil
}
