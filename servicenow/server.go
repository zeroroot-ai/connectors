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

// The tools use the Table API of ServiceNow on the table incident:
//   - GET  /api/now/table/incident                 (list, sysparm_query, sysparm_limit, sysparm_fields)
//   - POST /api/now/table/incident                 (create, 201)
//   - GET  /api/now/table/incident/{sys_id}        (read)
//   - PATCH /api/now/table/incident/{sys_id}       (update, 200)
// Reference: https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html
// The answer is {"result": ...}. A list is an array and one record is an object.
// A field of a record is a string. The state values 6 (Resolved) and 7 (Closed)
// are in https://www.servicenow.com/docs/r/7LhbZn3yz3Q58qhjZoU9iQ/nCIOdm4VSGlySPh~AEaryw.
//
// The tool names, the arguments and the results follow internal/ticket. The
// mapping to ServiceNow:
//   - ticket_id is sys_id (32 hex characters). number is the number INC0010001.
//   - queue is the sys_id of the assignment group.
//   - priority is set with urgency and impact, because ServiceNow computes the
//     priority from them: critical 1/1, high 1/2, medium 2/2, low 3/3.
//   - add_comment writes a work note (field work_notes), not a comment that the
//     caller of the incident sees.
//   - close_ticket sets state 7 with close_code and close_notes in one request.
//   - labels have no field on the incident table. A call with labels fails.

const (
	table = "incident"
	// fields is the list of fields that the read tools ask for.
	fields = "sys_id,number,short_description,description,state,priority,opened_at,sys_updated_on,closed_at"
	// closedState is the state Closed. ServiceNow treats 7 and higher as inactive.
	closedState = "7"
	// resolvedState is the first state that get_ticket and search_tickets count
	// as closed: 6 (Resolved).
	resolvedState = "6"
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("SERVICENOW_INSTANCE_URL")
	user := env.Require("SERVICENOW_USER")
	pass := env.Secret("SERVICENOW_PASSWORD")
	closeCode := env.Optional("SERVICENOW_CLOSE_CODE", "Solved (Permanently)")
	timeout := env.Duration("SERVICENOW_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("SERVICENOW_INSTANCE_URL must use https")
	}
	if err := kit.CheckString("SERVICENOW_CLOSE_CODE", closeCode, 100, nil); err != nil {
		return nil, fmt.Errorf("configure close code: %w", err)
	}
	client := kit.NewClient(base, kit.ClientOptions{
		Auth: kit.BasicAuth(user, pass), Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	svc := &service{client: client, web: *base, closeCode: closeCode}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	client    *kit.Client
	web       url.URL
	closeCode string
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolCreate,
		Description: "Open one ServiceNow incident. Give title and description. queue is the sys_id of the assignment group. " +
			"priority is low, medium, high or critical. ServiceNow has no labels on an incident, so labels is refused. " +
			"dedup_key makes a repeat safe: a second call with the same key returns the first incident with existing true. " +
			"No tool deletes the incident. Undo: call close_ticket with the ticket_id of the result.",
		Annotations: kit.Reversible("Create ServiceNow incident", false),
	}, v.create)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolUpdate,
		Description: "Change the title, the description or the priority of one ServiceNow incident. Give at least one. " +
			"Undo: call update_ticket with the old value.",
		Annotations: kit.Reversible("Update ServiceNow incident", true),
	}, v.update)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolComment,
		Description: "Add a work note to one ServiceNow incident. A work note is internal: the caller of the incident does not see it. " +
			"No tool undoes this action: ServiceNow keeps the note in the journal of the incident.",
		Annotations: kit.Irreversible("Add ServiceNow work note", false),
	}, v.comment)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolClose,
		Description: "Close one ServiceNow incident: state 7 with the close code of the server and the resolution as the close notes. " +
			"ServiceNow can refuse a close from the current state, and then the tool shows its error. " +
			"No tool undoes this action: no tool of this server reopens the incident, and only a person does.",
		Annotations: kit.Irreversible("Close ServiceNow incident", true),
	}, v.close)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        ticket.ToolGet,
		Description: "Read one ServiceNow incident. Read only. status is the state number of ServiceNow. closed is true for state 6 and higher.",
		Annotations: kit.ReadOnly("Get ServiceNow incident"),
	}, v.get)
	kit.AddTool(s, log, &mcp.Tool{
		Name: ticket.ToolSearch,
		Description: "Search ServiceNow incidents with typed filters: queue, status (open, closed or any), title_contains, " +
			"created_after and limit. Newest first. Read only. A title text that holds the character ^ is refused. " +
			"The label filter is refused, because ServiceNow has no labels on an incident.",
		Annotations: kit.ReadOnly("Search ServiceNow incidents"),
	}, v.search)
}

// ---- mapping ------------------------------------------------------------

// urgencyImpact maps a priority of the contract to the urgency and the impact.
var urgencyImpact = map[string][2]string{
	ticket.Critical: {"1", "1"},
	ticket.High:     {"1", "2"},
	ticket.Medium:   {"2", "2"},
	ticket.Low:      {"3", "3"},
}

func priorityName(p string) string {
	switch p {
	case "1":
		return ticket.Critical
	case "2":
		return ticket.High
	case "3":
		return ticket.Medium
	case "4", "5":
		return ticket.Low
	}
	return ""
}

type record struct {
	SysID            string `json:"sys_id"`
	Number           string `json:"number"`
	ShortDescription string `json:"short_description"`
	Description      string `json:"description"`
	State            string `json:"state"`
	Priority         string `json:"priority"`
	OpenedAt         string `json:"opened_at"`
	UpdatedOn        string `json:"sys_updated_on"`
	ClosedAt         string `json:"closed_at"`
}

// closed is true for state 6 (Resolved) and higher.
func (r record) closed() bool {
	n, err := strconv.Atoi(r.State)
	return err == nil && n >= 6
}

func (v *service) toTicket(r record) ticket.Ticket {
	return ticket.Ticket{
		TicketID: r.SysID, Number: r.Number, URL: v.recordURL(r.SysID), Title: r.ShortDescription,
		Description: r.Description, Status: r.State, Priority: priorityName(r.Priority), Closed: r.closed(),
		CreatedAt: sysTime(r.OpenedAt), UpdatedAt: sysTime(r.UpdatedOn),
	}
}

// sysTime reads a date and time of ServiceNow ("2026-11-20 10:00:00", UTC) as
// RFC 3339. A value that does not parse is returned as it is.
func sysTime(s string) string {
	if t, err := time.Parse(time.DateTime, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}

// recordURL is the address of the incident in the web interface of ServiceNow.
func (v *service) recordURL(sysID string) string {
	u := v.web
	u.Path = strings.TrimRight(u.Path, "/") + "/incident.do"
	u.RawQuery = url.Values{"sys_id": {sysID}}.Encode()
	return u.String()
}

func recordPath(id string) string { return kit.Path("api", "now", "table", table, id) }

func checkID(field, id string) error {
	if err := kit.CheckString(field, id, 32, kit.Hex32); err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	return nil
}

// snError shows the error message of ServiceNow when the answer has one.
func snError(op string, err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var body struct {
			Error struct {
				Message string `json:"message"`
				Detail  string `json:"detail"`
			} `json:"error"`
		}
		if kit.DecodeErrorBody(ae, &body) == nil && body.Error.Message != "" {
			return fmt.Errorf("%s: ServiceNow answered HTTP %d: %s: %s", op, ae.Status, body.Error.Message, body.Error.Detail)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// codeTag matches the tags [code] and [/code]. ServiceNow renders the text between
// them as HTML in a journal field and in some fields of a form, so a text of a
// finding could put markup in the view of a person.
var codeTag = regexp.MustCompile(`(?i)\[(/?)code\]`)

// plain neutralizes the code tags of ServiceNow in a text that goes to a field.
// The tag gets a space inside the brackets, so it shows as text.
func plain(s string) string { return codeTag.ReplaceAllString(s, "[ ${1}code ]") }

// ---- tools --------------------------------------------------------------

type single struct {
	Result record `json:"result"`
}

func (v *service) create(ctx context.Context, _ *mcp.CallToolRequest, in ticket.CreateInput) (*mcp.CallToolResult, ticket.Created, error) {
	var out ticket.Created
	if err := ticket.CheckCreate(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	if len(in.Labels) > 0 {
		return nil, out, fmt.Errorf("%w: ServiceNow has no labels on an incident. Leave labels out", kit.ErrInvalidInput)
	}
	prio := in.Priority
	if prio == "" {
		prio = ticket.Medium
	}
	body := map[string]any{
		"short_description": plain(in.Title), "description": plain(in.Description),
		"urgency": urgencyImpact[prio][0], "impact": urgencyImpact[prio][1],
	}
	if in.Queue != "" {
		if err := checkID("queue", in.Queue); err != nil {
			return nil, out, err
		}
		body["assignment_group"] = in.Queue
	}
	if in.DedupKey != "" {
		body["correlation_id"] = in.DedupKey
		existing, err := v.byDedupKey(ctx, in.DedupKey)
		if err != nil {
			return nil, out, err
		}
		if existing != nil {
			return nil, ticket.Created{TicketID: existing.SysID, Number: existing.Number, URL: v.recordURL(existing.SysID), Existing: true}, nil
		}
	}
	var resp single
	// A create call is not repeated by the client: a repeat after a timeout
	// would open a second incident. The caller repeats it with the same dedup_key.
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("api", "now", "table", table),
		Query: url.Values{"sysparm_fields": {fields}, "sysparm_exclude_reference_link": {"true"}}, JSON: body,
	}, &resp)
	if err != nil {
		cerr := snError("create incident", err)
		if in.DedupKey != "" {
			// A repeat with the same dedup_key finds the incident that the lost call
			// made, so a repeat is safe.
			return nil, out, kit.Retryable(cerr)
		}
		return nil, out, cerr
	}
	if kit.CheckString("sys_id", resp.Result.SysID, 32, kit.Hex32) != nil {
		return nil, out, errors.New("create incident: ServiceNow gave no sys_id in the answer")
	}
	return nil, ticket.Created{TicketID: resp.Result.SysID, Number: resp.Result.Number, URL: v.recordURL(resp.Result.SysID)}, nil
}

func (v *service) patch(ctx context.Context, op, id string, body map[string]any) (record, error) {
	var resp single
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPatch, Path: recordPath(id),
		Query: url.Values{"sysparm_fields": {fields}, "sysparm_exclude_reference_link": {"true"}}, JSON: body,
		Retry: true, // the same values twice have the same effect
	}, &resp)
	if err != nil {
		return record{}, snError(op, err)
	}
	if resp.Result.SysID != id {
		return record{}, fmt.Errorf("%s: ServiceNow did not return the incident", op)
	}
	return resp.Result, nil
}

func (v *service) update(ctx context.Context, _ *mcp.CallToolRequest, in ticket.UpdateInput) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: ticket.ToolUpdate}
	if err := checkID("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := ticket.CheckUpdate(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	body := map[string]any{}
	if in.Title != "" {
		body["short_description"] = plain(in.Title)
	}
	if in.Description != "" {
		body["description"] = plain(in.Description)
	}
	if in.Priority != "" {
		body["urgency"], body["impact"] = urgencyImpact[in.Priority][0], urgencyImpact[in.Priority][1]
	}
	rec, err := v.patch(ctx, "update incident", in.TicketID, body)
	if err != nil {
		return nil, out, err
	}
	out.Status = rec.State
	return nil, out, nil
}

func (v *service) comment(ctx context.Context, _ *mcp.CallToolRequest, in ticket.CommentInput) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: ticket.ToolComment}
	if err := checkID("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := ticket.CheckText("body", in.Body, ticket.MaxComment); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	// A work note is added by each request, so the call is not repeated.
	var resp single
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPatch, Path: recordPath(in.TicketID),
		Query: url.Values{"sysparm_fields": {fields}, "sysparm_exclude_reference_link": {"true"}},
		JSON:  map[string]any{"work_notes": plain(in.Body)},
	}, &resp)
	if err != nil {
		return nil, out, snError("add work note", err)
	}
	out.Status = resp.Result.State
	return nil, out, nil
}

func (v *service) close(ctx context.Context, _ *mcp.CallToolRequest, in ticket.CloseInput) (*mcp.CallToolResult, ticket.Changed, error) {
	out := ticket.Changed{TicketID: in.TicketID, Action: ticket.ToolClose}
	if err := checkID("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	if err := ticket.CheckText("resolution", in.Resolution, ticket.MaxComment); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	rec, err := v.patch(ctx, "close incident", in.TicketID, map[string]any{
		"state": closedState, "close_code": v.closeCode, "close_notes": plain(in.Resolution),
	})
	if err != nil {
		return nil, out, err
	}
	// ServiceNow can answer 200 and keep the old state. Check the answer.
	if rec.State != closedState {
		return nil, out, fmt.Errorf("close incident: ServiceNow answered with state %q and did not close the incident", rec.State)
	}
	out.Status = rec.State
	return nil, out, nil
}

func (v *service) get(ctx context.Context, _ *mcp.CallToolRequest, in ticket.GetInput) (*mcp.CallToolResult, ticket.Ticket, error) {
	var out ticket.Ticket
	if err := checkID("ticket_id", in.TicketID); err != nil {
		return nil, out, err
	}
	var resp single
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: recordPath(in.TicketID),
		Query: url.Values{"sysparm_fields": {fields}, "sysparm_exclude_reference_link": {"true"}},
	}, &resp)
	if err != nil {
		return nil, out, snError("get incident", err)
	}
	if resp.Result.SysID != in.TicketID {
		return nil, out, errors.New("get incident: ServiceNow did not return the incident")
	}
	return nil, v.toTicket(resp.Result), nil
}

// queryTime is the format of a time in an encoded query of ServiceNow (UTC).
const queryTime = "2006-01-02 15:04:05"

// encodedQuery builds sysparm_query from the typed filters. The character ^
// separates conditions in this language and has no escape that the Table API
// documents, so a text with ^ is refused. The ids are 32 hex characters.
func encodedQuery(in ticket.SearchInput) (string, error) {
	if in.Label != "" {
		return "", fmt.Errorf("%w: ServiceNow has no labels on an incident. Leave label out", kit.ErrInvalidInput)
	}
	var conds []string
	switch ticket.StatusOf(in) {
	// The same rule as record.closed: state 6 (Resolved) and higher is closed.
	case ticket.StatusOpen:
		conds = append(conds, "state<"+resolvedState)
	case ticket.StatusClosed:
		conds = append(conds, "state>="+resolvedState)
	}
	if in.Queue != "" {
		if err := checkID("queue", in.Queue); err != nil {
			return "", err
		}
		conds = append(conds, "assignment_group="+in.Queue)
	}
	if in.TitleContains != "" {
		if strings.Contains(in.TitleContains, "^") {
			return "", fmt.Errorf("%w: title_contains must not hold the character ^", kit.ErrInvalidInput)
		}
		// ServiceNow runs a value that starts with javascript: as a script in an
		// encoded query. The tool refuses the text at any place.
		if strings.Contains(strings.ToLower(in.TitleContains), "javascript:") {
			return "", fmt.Errorf("%w: title_contains must not hold the text javascript", kit.ErrInvalidInput)
		}
		conds = append(conds, "short_descriptionLIKE"+in.TitleContains)
	}
	if in.DedupKey != "" {
		// The key has a fixed pattern. The tool still refuses ^ and javascript: here,
		// so a change of the pattern cannot add a condition to the query.
		if strings.Contains(in.DedupKey, "^") || strings.Contains(strings.ToLower(in.DedupKey), "javascript:") {
			return "", fmt.Errorf("%w: dedup_key must not hold the character ^ or the text javascript", kit.ErrInvalidInput)
		}
		conds = append(conds, "correlation_id="+in.DedupKey)
	}
	if in.CreatedAfter != "" {
		t, err := time.Parse(time.RFC3339, in.CreatedAfter)
		if err != nil {
			return "", fmt.Errorf("%w: created_after is not an RFC 3339 time", kit.ErrInvalidInput)
		}
		conds = append(conds, "opened_at>="+t.UTC().Format(queryTime))
	}
	if in.DedupKey != "" {
		conds = append(conds, "ORDERBYopened_at") // oldest first
	} else {
		conds = append(conds, "ORDERBYDESCopened_at")
	}
	return strings.Join(conds, "^"), nil
}

func (v *service) search(ctx context.Context, _ *mcp.CallToolRequest, in ticket.SearchInput) (*mcp.CallToolResult, ticket.SearchOutput, error) {
	out := ticket.SearchOutput{Tickets: []ticket.Ticket{}}
	limit, err := ticket.CheckSearch(in)
	if err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	q, err := encodedQuery(in)
	if err != nil {
		return nil, out, err
	}
	var resp struct {
		Result []record `json:"result"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("api", "now", "table", table),
		Query: url.Values{
			"sysparm_query": {q}, "sysparm_limit": {strconv.Itoa(limit)}, "sysparm_fields": {fields},
			"sysparm_exclude_reference_link": {"true"},
		},
	}, &resp)
	if err != nil {
		return nil, out, snError("search incidents", err)
	}
	for _, r := range resp.Result {
		out.Tickets = append(out.Tickets, v.toTicket(r))
	}
	out.Count = len(out.Tickets)
	return nil, out, nil
}

// byDedupKey returns the incident that holds the dedup key in correlation_id.
// The key has a fixed pattern, so it cannot add a condition to the query. The
// oldest incident wins, and the search does not look at the state, because a
// retry must get the same incident back after someone closed it.
func (v *service) byDedupKey(ctx context.Context, key string) (*record, error) {
	var resp struct {
		Result []record `json:"result"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("api", "now", "table", table),
		Query: url.Values{
			"sysparm_query": {"correlation_id=" + key + "^ORDERBYopened_at"}, "sysparm_limit": {"1"}, "sysparm_fields": {fields},
			"sysparm_exclude_reference_link": {"true"},
		},
	}, &resp)
	if err != nil {
		return nil, snError("look for the dedup key", err)
	}
	if len(resp.Result) == 0 {
		return nil, nil
	}
	return &resp.Result[0], nil
}
