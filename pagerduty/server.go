package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools use two PagerDuty APIs:
//   - Events API v2: POST /v2/enqueue on https://events.pagerduty.com. The body
//     is {routing_key, event_action (trigger, acknowledge, resolve), dedup_key,
//     payload{summary, source, severity, component, group, class, custom_details}}.
//     The answer is HTTP 202 {"status":"success","message":"Event processed","dedup_key"}.
//     summary has at most 1024 characters. dedup_key has at most 255 and is
//     required for acknowledge and resolve.
//     https://docs.pagerduty.com/developer/api/reference/events-v2/send-event/create-v2-event
//     https://docs.pagerduty.com/developer/send-alert-event.md
//   - REST API v2: GET /incidents/{id} and GET /incidents?incident_key= on
//     https://api.pagerduty.com. Headers: Accept: application/vnd.pagerduty+json;version=2
//     and Authorization: Token token=<key>.
//     https://docs.pagerduty.com/developer/api/reference/rest/incidents/get-incident
//     https://docs.pagerduty.com/developer/api/reference/rest/incidents/list-incidents
// An alert is not an incident. The Events API makes an alert, and PagerDuty
// makes an incident from it. The routing key belongs to one integration of one
// service, so one server serves one service.

var (
	dedupFormat    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@=+-]*$`)
	incidentFormat = regexp.MustCompile(`^[A-Z0-9]{1,20}$`)
	keyFormat      = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	fieldFormat    = regexp.MustCompile(`^[A-Za-z0-9._ :/-]{1,100}$`)
)

const (
	maxSummary = 1024
	maxDedup   = 255
	maxDetails = 20
	maxDetail  = 500
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	routing := env.Secret("PAGERDUTY_ROUTING_KEY")
	token := env.Secret("PAGERDUTY_API_TOKEN")
	eventsURL := env.OptionalURL("PAGERDUTY_EVENTS_URL", "https://events.pagerduty.com")
	apiURL := env.OptionalURL("PAGERDUTY_API_URL", "https://api.pagerduty.com")
	timeout := env.Duration("PAGERDUTY_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if eventsURL.Scheme != "https" || apiURL.Scheme != "https" {
		return nil, errors.New("PAGERDUTY_EVENTS_URL and PAGERDUTY_API_URL must use https")
	}
	if !keyFormat.MatchString(routing.Reveal()) {
		return nil, errors.New("PAGERDUTY_ROUTING_KEY has the wrong format")
	}
	rest := kit.NewClient(apiURL, kit.ClientOptions{
		Auth: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")
			req.Header.Set("Authorization", "Token token="+token.Reveal())
			return nil
		},
		Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	events := kit.NewClient(eventsURL, kit.ClientOptions{Logger: log, Timeout: timeout, HTTPClient: hc})
	svc := &service{events: events, rest: rest, routing: routing}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	events, rest *kit.Client
	routing      kit.Secret
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "trigger_alert",
		Description: "Page the on-call person: send a trigger event to the PagerDuty service of this server. " +
			"dedup_key is required. A second trigger with the same dedup_key adds to the open alert and does not page again. " +
			"After the alert is resolved, the same dedup_key opens a new alert. " +
			"Undo: call resolve_alert with the same dedup_key. The page that PagerDuty already sent stays sent.",
		Annotations: kit.Reversible("Trigger PagerDuty alert", true),
	}, v.trigger)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "acknowledge_alert",
		Description: "Acknowledge the open alert with this dedup_key. The alert must exist on the service of this server. " +
			"PagerDuty drops the event for an unknown or resolved key. " +
			"No tool undoes this action: the acknowledgement ends by timeout in PagerDuty, or a person changes it.",
		Annotations: kit.Irreversible("Acknowledge PagerDuty alert", true),
	}, v.acknowledge)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "resolve_alert",
		Description: "Resolve the open alert with this dedup_key. " +
			"No tool undoes this action: a later trigger with the same dedup_key opens a new alert and does not reopen this one.",
		Annotations: kit.Irreversible("Resolve PagerDuty alert", true),
	}, v.resolve)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "get_incident",
		Description: "Read one PagerDuty incident by its id (for example PT4KHLK): status, urgency, service, times and the dedup key. Read only.",
		Annotations: kit.ReadOnly("Get PagerDuty incident"),
	}, v.getIncident)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "find_incidents",
		Description: "Find the incidents of one dedup_key. The Events API gives a dedup_key and no incident id, so call this to get the incident. " +
			"Read only. Returns at most 10 incidents, in any status.",
		Annotations: kit.ReadOnly("Find PagerDuty incidents by dedup key"),
	}, v.findIncidents)
}

// ---- events ------------------------------------------------------------

type triggerInput struct {
	Summary       string            `json:"summary" jsonschema:"what happened, at most 1024 characters"`
	Source        string            `json:"source" jsonschema:"the system that has the problem, for example a host name or a service"`
	DedupKey      string            `json:"dedup_key" jsonschema:"the key of the alert, at most 255 characters. The same key groups the events of one alert."`
	Severity      string            `json:"severity,omitempty" jsonschema:"critical, error, warning or info. The default is error."`
	Component     string            `json:"component,omitempty"`
	Group         string            `json:"group,omitempty"`
	Class         string            `json:"class,omitempty"`
	CustomDetails map[string]string `json:"custom_details,omitempty" jsonschema:"up to 20 pairs of text, shown in the alert"`
}

type dedupInput struct {
	DedupKey string `json:"dedup_key" jsonschema:"the dedup_key of the alert, at most 255 characters"`
}

type eventOutput struct {
	DedupKey string `json:"dedup_key"`
	Action   string `json:"action"`
	Message  string `json:"message,omitempty"`
}

func checkDedup(k string) error {
	if err := kit.CheckString("dedup_key", k, maxDedup, dedupFormat); err != nil {
		return fmt.Errorf("check dedup_key: %w", err)
	}
	return nil
}

// send posts one event. The same event twice has the same effect with a
// dedup_key, so the call is repeated on a temporary error.
func (v *service) send(ctx context.Context, action, dedup string, payload map[string]any) (eventOutput, error) {
	out := eventOutput{DedupKey: dedup, Action: action}
	body := map[string]any{"routing_key": v.routing.Reveal(), "event_action": action, "dedup_key": dedup}
	if payload != nil {
		body["payload"] = payload
	}
	var resp struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		DedupKey string `json:"dedup_key"`
	}
	err := v.events.Do(ctx, kit.Request{Method: http.MethodPost, Path: kit.Path("v2", "enqueue"), JSON: body, Retry: true}, &resp)
	if err != nil {
		return out, fmt.Errorf("%s: %w", action, err)
	}
	if resp.Status != "success" {
		return out, fmt.Errorf("%s: PagerDuty answered status %q: %s", action, resp.Status, resp.Message)
	}
	if resp.DedupKey != "" && resp.DedupKey != dedup {
		return out, fmt.Errorf("%s: PagerDuty answered with another dedup_key", action)
	}
	out.Message = resp.Message
	return out, nil
}

func (v *service) trigger(ctx context.Context, _ *mcp.CallToolRequest, in triggerInput) (*mcp.CallToolResult, eventOutput, error) {
	out := eventOutput{DedupKey: in.DedupKey, Action: "trigger"}
	if err := checkDedup(in.DedupKey); err != nil {
		return nil, out, err
	}
	if err := kit.CheckString("summary", in.Summary, maxSummary, nil); err != nil {
		return nil, out, fmt.Errorf("check summary: %w", err)
	}
	if err := kit.CheckString("source", in.Source, 255, nil); err != nil {
		return nil, out, fmt.Errorf("check source: %w", err)
	}
	sev := in.Severity
	switch sev {
	case "":
		sev = "error"
	case "critical", "error", "warning", "info":
	default:
		return nil, out, fmt.Errorf("%w: severity must be critical, error, warning or info", kit.ErrInvalidInput)
	}
	payload := map[string]any{"summary": in.Summary, "source": in.Source, "severity": sev}
	for k, val := range map[string]string{"component": in.Component, "group": in.Group, "class": in.Class} {
		if val == "" {
			continue
		}
		if err := kit.CheckString(k, val, 255, nil); err != nil {
			return nil, out, fmt.Errorf("check %s: %w", k, err)
		}
		payload[k] = val
	}
	if len(in.CustomDetails) > maxDetails {
		return nil, out, fmt.Errorf("%w: custom_details has more than %d pairs", kit.ErrInvalidInput, maxDetails)
	}
	if len(in.CustomDetails) > 0 {
		for k, val := range in.CustomDetails {
			if !fieldFormat.MatchString(k) {
				return nil, out, fmt.Errorf("%w: a custom_details key has the wrong format", kit.ErrInvalidInput)
			}
			if err := kit.CheckString("custom_details value", val, maxDetail, nil); err != nil {
				return nil, out, fmt.Errorf("check custom_details: %w", err)
			}
		}
		payload["custom_details"] = in.CustomDetails
	}
	res, err := v.send(ctx, "trigger", in.DedupKey, payload)
	return nil, res, err
}

func (v *service) acknowledge(ctx context.Context, _ *mcp.CallToolRequest, in dedupInput) (*mcp.CallToolResult, eventOutput, error) {
	return v.simple(ctx, "acknowledge", in)
}

func (v *service) resolve(ctx context.Context, _ *mcp.CallToolRequest, in dedupInput) (*mcp.CallToolResult, eventOutput, error) {
	return v.simple(ctx, "resolve", in)
}

func (v *service) simple(ctx context.Context, action string, in dedupInput) (*mcp.CallToolResult, eventOutput, error) {
	out := eventOutput{DedupKey: in.DedupKey, Action: action}
	if err := checkDedup(in.DedupKey); err != nil {
		return nil, out, err
	}
	res, err := v.send(ctx, action, in.DedupKey, nil)
	return nil, res, err
}

// ---- incidents ---------------------------------------------------------

type incident struct {
	ID                 string `json:"id"`
	IncidentNumber     int    `json:"incident_number"`
	Title              string `json:"title"`
	Status             string `json:"status"`
	Urgency            string `json:"urgency"`
	HTMLURL            string `json:"html_url"`
	CreatedAt          string `json:"created_at"`
	LastStatusChangeAt string `json:"last_status_change_at"`
	IncidentKey        string `json:"incident_key"`
	Service            struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"service"`
	Priority *struct {
		Summary string `json:"summary"`
	} `json:"priority"`
}

type incidentOut struct {
	ID                 string `json:"incident_id"`
	Number             int    `json:"incident_number"`
	Title              string `json:"title"`
	Status             string `json:"status"`
	Urgency            string `json:"urgency,omitempty"`
	URL                string `json:"url,omitempty"`
	CreatedAt          string `json:"created_at,omitempty"`
	LastStatusChangeAt string `json:"last_status_change_at,omitempty"`
	DedupKey           string `json:"dedup_key,omitempty"`
	ServiceID          string `json:"service_id,omitempty"`
	Service            string `json:"service,omitempty"`
	Priority           string `json:"priority,omitempty"`
}

func (i incident) out() incidentOut {
	o := incidentOut{
		ID: i.ID, Number: i.IncidentNumber, Title: i.Title, Status: i.Status, Urgency: i.Urgency, URL: i.HTMLURL,
		CreatedAt: i.CreatedAt, LastStatusChangeAt: i.LastStatusChangeAt, DedupKey: i.IncidentKey,
		ServiceID: i.Service.ID, Service: i.Service.Summary,
	}
	if i.Priority != nil {
		o.Priority = i.Priority.Summary
	}
	return o
}

type incidentInput struct {
	IncidentID string `json:"incident_id" jsonschema:"the incident id: capital letters and digits, for example PT4KHLK"`
}

func (v *service) getIncident(ctx context.Context, _ *mcp.CallToolRequest, in incidentInput) (*mcp.CallToolResult, incidentOut, error) {
	var out incidentOut
	if err := kit.CheckString("incident_id", in.IncidentID, 20, incidentFormat); err != nil {
		return nil, out, fmt.Errorf("check incident_id: %w", err)
	}
	var resp struct {
		Incident incident `json:"incident"`
	}
	if err := v.rest.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("incidents", in.IncidentID)}, &resp); err != nil {
		return nil, out, fmt.Errorf("get incident: %w", err)
	}
	if resp.Incident.ID != in.IncidentID && fmt.Sprint(resp.Incident.IncidentNumber) != in.IncidentID {
		return nil, out, errors.New("get incident: PagerDuty did not return the incident")
	}
	return nil, resp.Incident.out(), nil
}

type findOutput struct {
	Incidents []incidentOut `json:"incidents"`
	Count     int           `json:"count"`
}

func (v *service) findIncidents(ctx context.Context, _ *mcp.CallToolRequest, in dedupInput) (*mcp.CallToolResult, findOutput, error) {
	out := findOutput{Incidents: []incidentOut{}}
	if err := checkDedup(in.DedupKey); err != nil {
		return nil, out, err
	}
	var resp struct {
		Incidents []incident `json:"incidents"`
	}
	err := v.rest.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("incidents"),
		Query: url.Values{"incident_key": {in.DedupKey}, "date_range": {"all"}, "limit": {"10"}},
	}, &resp)
	if err != nil {
		return nil, out, fmt.Errorf("find incidents: %w", err)
	}
	for _, i := range resp.Incidents {
		out.Incidents = append(out.Incidents, i.out())
	}
	out.Count = len(out.Incidents)
	return nil, out, nil
}
