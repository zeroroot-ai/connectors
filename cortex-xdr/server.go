package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools use the public API of Cortex XDR. The pages are on
// cortex-docs.paloaltonetworks.com:
//   - POST /public_api/v1/incidents/get_incidents
//   - POST /public_api/v1/incidents/get_incident_extra_data
//     https://cortex-docs.paloaltonetworks.com/xdr-3-api/cortex-xdr-3.x-apis/incident-management
//   - POST /public_api/v1/alerts/get_alerts
//     (the same page, section Get Alerts)
//   - POST /public_api/v1/endpoints/get_endpoint
//     https://cortex-docs.paloaltonetworks.com/xdr-3-api/cortex-xdr-3.x-apis/endpoint-management
//   - POST /public_api/v1/endpoints/isolate and /endpoints/unisolate
//     https://cortex-docs.paloaltonetworks.com/xdr-3-api/cortex-xdr-3.x-apis/response-action.md
//     The body is {"request_data": {"endpoint_id": "<id>", "incident_id": "<optional>"}}.
//     The page allows a list of endpoints with filters instead, and refuses both
//     together. The tools send endpoint_id only, so one call changes one endpoint.
//     The answer is {"reply": {"action_id": "...", "endpoints_count": "1"}}. It says only
//     that the request went to the endpoint. Both calls need the license Cortex XDR
//     Prevent or Pro per Endpoint.
// Each call takes {"request_data": {...}} and answers {"reply": {...}}. A filter
// is {"field","operator","value"}. A page is search_from and search_to, at most
// 100 records. The base URL is https://api-{fqdn}, where the fqdn belongs to the
// tenant. Authentication, from
// https://cortex-docs.paloaltonetworks.com/xdr-3-api/get-started-with-cortex-xdr-apis.md:
//   standard: x-xdr-auth-id: <key id>, Authorization: <key>
//   advanced: x-xdr-auth-id, x-xdr-nonce (64 letters and digits), x-xdr-timestamp
//   (epoch milliseconds, UTC) and Authorization: sha256 hex of key+nonce+timestamp.
// The API allows 10 requests a second for each tenant.

const (
	maxLimit     = 100
	defaultLimit = 25
	maxOffset    = 10000
	nonceLen     = 64
)

var (
	incidentFormat = regexp.MustCompile(`^\d{1,20}$`)
	endpointFormat = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	hostFormat     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	keyIDFormat    = regexp.MustCompile(`^\d{1,10}$`)
)

var (
	incidentStatuses = map[string]bool{
		"new": true, "under_investigation": true, "resolved_known_issue": true, "resolved_duplicate": true, "resolved_false_positive": true,
		"resolved_true_positive": true, "resolved_security_testing": true, "resolved_threat_handled": true, "resolved_auto": true, "resolved_other": true,
	}
	severities       = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}
	endpointStatuses = map[string]bool{"connected": true, "disconnected": true, "lost": true, "uninstalled": true}
	isolations       = map[string]bool{"isolated": true, "unisolated": true}
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("CORTEX_API_URL")
	keyID := env.Require("CORTEX_API_KEY_ID")
	key := env.Secret("CORTEX_API_KEY")
	keyType := env.Optional("CORTEX_API_KEY_TYPE", "standard")
	timeout := env.Duration("CORTEX_TIMEOUT", 30*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("CORTEX_API_URL must use https")
	}
	if !keyIDFormat.MatchString(keyID) {
		return nil, errors.New("CORTEX_API_KEY_ID must be a number")
	}
	if keyType != "standard" && keyType != "advanced" {
		return nil, errors.New("CORTEX_API_KEY_TYPE must be standard or advanced")
	}
	api := *base
	api.Path += "/public_api"
	client := kit.NewClient(&api, kit.ClientOptions{
		Auth: authenticator(keyID, key, keyType == "advanced", time.Now), Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

// authenticator signs a request. The standard key goes in the header as it
// is. The advanced key is never sent: the header holds the SHA-256 of the key,
// a nonce and a timestamp.
func authenticator(keyID string, key kit.Secret, advanced bool, now func() time.Time) kit.Authenticator {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set("x-xdr-auth-id", keyID)
		if !advanced {
			req.Header.Set("Authorization", key.Reveal())
			return nil
		}
		nonce, err := newNonce()
		if err != nil {
			return fmt.Errorf("make nonce: %w", err)
		}
		ts := strconv.FormatInt(now().UnixMilli(), 10)
		sum := sha256.Sum256([]byte(key.Reveal() + nonce + ts))
		req.Header.Set("x-xdr-nonce", nonce)
		req.Header.Set("x-xdr-timestamp", ts)
		req.Header.Set("Authorization", hex.EncodeToString(sum[:]))
		return nil
	}
}

const nonceChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func newNonce() (string, error) {
	b := make([]byte, nonceLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(nonceChars))))
		if err != nil {
			return "", fmt.Errorf("read random: %w", err)
		}
		b[i] = nonceChars[n.Int64()]
	}
	return string(b), nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_incidents",
		Description: "List Cortex XDR incidents, newest first: id, description, status, severity, host and alert counts, and the assignee. Read only. " +
			"All inputs are optional. status is one of new, under_investigation, resolved_known_issue, resolved_duplicate, resolved_false_positive, " +
			"resolved_true_positive, resolved_security_testing, resolved_threat_handled, resolved_auto or resolved_other. " +
			"created_after and created_before are RFC 3339 times. Times in the result are epoch milliseconds.",
		Annotations: kit.ReadOnly("List Cortex XDR incidents"),
	}, v.listIncidents)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_incident",
		Description: "Read one Cortex XDR incident with its alerts. Read only. The API allows 10 of these calls a minute for the tenant. " +
			"Returns at most alerts_limit alerts, 25 by default and 100 at most.",
		Annotations: kit.ReadOnly("Get Cortex XDR incident"),
	}, v.getIncident)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_alerts",
		Description: "List Cortex XDR alerts, newest first: name, category, severity, source, action, host and the MITRE tactic and technique. Read only. " +
			"All inputs are optional. severity is low, medium, high or critical. created_after and created_before are RFC 3339 times.",
		Annotations: kit.ReadOnly("List Cortex XDR alerts"),
	}, v.listAlerts)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_endpoints",
		Description: "List the state of Cortex XDR endpoints: name, status, platform, addresses, last seen and whether the endpoint is isolated. Read only. " +
			"All inputs are optional. status is connected, disconnected, lost or uninstalled. isolation is isolated or unisolated. " +
			"To isolate or release one endpoint, use isolate_endpoint and unisolate_endpoint.",
		Annotations: kit.ReadOnly("List Cortex XDR endpoints"),
	}, v.listEndpoints)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "isolate_endpoint",
		Description: "Response action. Isolate one Cortex XDR endpoint from the network. The agent of the endpoint keeps its link to Cortex XDR. " +
			"The answer shows only that the request went to the endpoint. The endpoint can be offline, and then the isolation starts when it connects. " +
			"Read is_isolated with list_endpoints to see the result. Needs the license Cortex XDR Prevent or Pro per Endpoint. " +
			"Undo: call unisolate_endpoint with the same endpoint_id.",
		Annotations: kit.Reversible("Isolate Cortex XDR endpoint", true),
	}, v.isolate)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "unisolate_endpoint",
		Description: "Response action. Release one isolated Cortex XDR endpoint. The answer shows only that the request went to the endpoint. " +
			"This is the undo of isolate_endpoint.",
		Annotations: kit.Reversible("Unisolate Cortex XDR endpoint", true),
	}, v.unisolate)
}

// ---- request helpers ---------------------------------------------------

type page struct {
	Limit  int `json:"limit,omitempty" jsonschema:"number of records, 1 to 100. The default is 25."`
	Offset int `json:"offset,omitempty" jsonschema:"number of records to skip. The default is 0."`
}

func (p page) bounds() (from, to int, err error) {
	limit := p.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if err := kit.CheckRange("limit", limit, 1, maxLimit); err != nil {
		return 0, 0, fmt.Errorf("check limit: %w", err)
	}
	if err := kit.CheckRange("offset", p.Offset, 0, maxOffset); err != nil {
		return 0, 0, fmt.Errorf("check offset: %w", err)
	}
	return p.Offset, p.Offset + limit, nil
}

type filter struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

func enumFilter(field, op, name, val string, allowed map[string]bool) (*filter, error) {
	if val == "" {
		return nil, nil
	}
	if !allowed[val] {
		return nil, fmt.Errorf("%w: %s has a value that Cortex XDR does not know", kit.ErrInvalidInput, name)
	}
	return &filter{Field: field, Operator: op, Value: []string{val}}, nil
}

func timeFilter(field, op, name, val string) (*filter, error) {
	if val == "" {
		return nil, nil
	}
	if err := kit.CheckString(name, val, 40, nil); err != nil {
		return nil, fmt.Errorf("check %s: %w", name, err)
	}
	t, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not an RFC 3339 time", kit.ErrInvalidInput, name)
	}
	return &filter{Field: field, Operator: op, Value: t.UnixMilli()}, nil
}

func collect(fs ...*filter) []filter {
	out := []filter{}
	for _, f := range fs {
		if f != nil {
			out = append(out, *f)
		}
	}
	return out
}

// call sends one request. A search changes nothing, so the client repeats it.
func (v *service) call(ctx context.Context, op string, path []string, data map[string]any, reply any) error {
	return v.send(ctx, op, path, data, reply, true)
}

func (v *service) send(ctx context.Context, op string, path []string, data map[string]any, reply any, retry bool) error {
	var resp struct {
		Reply any `json:"reply"`
	}
	resp.Reply = reply
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path(append([]string{"v1"}, path...)...), JSON: map[string]any{"request_data": data}, Retry: retry,
	}, &resp)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// ---- incidents ---------------------------------------------------------

type incidentsIn struct {
	Status           string `json:"status,omitempty"`
	CreatedAfter     string `json:"created_after,omitempty"`
	CreatedBefore    string `json:"created_before,omitempty"`
	DescriptionHolds string `json:"description_contains,omitempty" jsonschema:"text that the description holds"`
	page
}

type incident struct {
	ID               string `json:"incident_id"`
	Name             string `json:"incident_name,omitempty"`
	Description      string `json:"description,omitempty"`
	Status           string `json:"status"`
	Severity         string `json:"severity,omitempty"`
	CreationTime     int64  `json:"creation_time,omitempty"`
	ModificationTime int64  `json:"modification_time,omitempty"`
	HostCount        int    `json:"host_count"`
	AlertCount       int    `json:"alert_count"`
	AssignedUser     string `json:"assigned_user_mail,omitempty"`
}

type incidentsOut struct {
	Incidents []incident `json:"incidents"`
	Count     int        `json:"count"`
	Total     int        `json:"total"`
	HasMore   bool       `json:"has_more"`
}

func (v *service) listIncidents(ctx context.Context, _ *mcp.CallToolRequest, in incidentsIn) (*mcp.CallToolResult, incidentsOut, error) {
	out := incidentsOut{Incidents: []incident{}}
	from, to, err := in.bounds()
	if err != nil {
		return nil, out, err
	}
	// The status filter of incidents takes one string with eq.
	var st *filter
	if in.Status != "" {
		if !incidentStatuses[in.Status] {
			return nil, out, fmt.Errorf("%w: status has a value that Cortex XDR does not know", kit.ErrInvalidInput)
		}
		st = &filter{Field: "status", Operator: "eq", Value: in.Status}
	}
	after, err := timeFilter("creation_time", "gte", "created_after", in.CreatedAfter)
	if err != nil {
		return nil, out, err
	}
	before, err := timeFilter("creation_time", "lte", "created_before", in.CreatedBefore)
	if err != nil {
		return nil, out, err
	}
	var desc *filter
	if in.DescriptionHolds != "" {
		if err := kit.CheckString("description_contains", in.DescriptionHolds, 200, nil); err != nil {
			return nil, out, fmt.Errorf("check description_contains: %w", err)
		}
		desc = &filter{Field: "description", Operator: "contains", Value: in.DescriptionHolds}
	}
	var reply struct {
		TotalCount int        `json:"total_count"`
		Incidents  []incident `json:"incidents"`
	}
	data := map[string]any{
		"filters": collect(st, after, before, desc), "search_from": from, "search_to": to,
		"sort": map[string]string{"field": "creation_time", "keyword": "desc"},
	}
	if err := v.call(ctx, "list incidents", []string{"incidents", "get_incidents"}, data, &reply); err != nil {
		return nil, out, err
	}
	if reply.Incidents != nil {
		out.Incidents = reply.Incidents
	}
	out.Count, out.Total = len(out.Incidents), reply.TotalCount
	out.HasMore = from+out.Count < out.Total
	return nil, out, nil
}

type incidentIn struct {
	IncidentID  string `json:"incident_id" jsonschema:"the incident id: digits"`
	AlertsLimit int    `json:"alerts_limit,omitempty" jsonschema:"alerts to return, 1 to 100. The default is 25."`
}

type alertSummary struct {
	AlertID            string `json:"alert_id"`
	Name               string `json:"name,omitempty"`
	Category           string `json:"category,omitempty"`
	Severity           string `json:"severity,omitempty"`
	Source             string `json:"source,omitempty"`
	Action             string `json:"action,omitempty"`
	DetectionTimestamp int64  `json:"detection_timestamp,omitempty"`
	EndpointID         string `json:"endpoint_id,omitempty"`
	HostName           string `json:"host_name,omitempty"`
	UserName           string `json:"user_name,omitempty"`
	Description        string `json:"description,omitempty"`
}

type incidentOut struct {
	Incident    incident       `json:"incident"`
	Alerts      []alertSummary `json:"alerts"`
	AlertsTotal int            `json:"alerts_total"`
}

func (v *service) getIncident(ctx context.Context, _ *mcp.CallToolRequest, in incidentIn) (*mcp.CallToolResult, incidentOut, error) {
	out := incidentOut{Alerts: []alertSummary{}}
	if err := kit.CheckString("incident_id", in.IncidentID, 20, incidentFormat); err != nil {
		return nil, out, fmt.Errorf("check incident_id: %w", err)
	}
	limit := in.AlertsLimit
	if limit == 0 {
		limit = defaultLimit
	}
	if err := kit.CheckRange("alerts_limit", limit, 1, maxLimit); err != nil {
		return nil, out, fmt.Errorf("check alerts_limit: %w", err)
	}
	var reply struct {
		Incident incident `json:"incident"`
		Alerts   struct {
			TotalCount int            `json:"total_count"`
			Data       []alertSummary `json:"data"`
		} `json:"alerts"`
	}
	data := map[string]any{"incident_id": in.IncidentID, "alerts_limit": limit}
	if err := v.call(ctx, "get incident", []string{"incidents", "get_incident_extra_data"}, data, &reply); err != nil {
		return nil, out, err
	}
	if reply.Incident.ID != in.IncidentID {
		return nil, out, errors.New("get incident: Cortex XDR did not return the incident")
	}
	out.Incident, out.AlertsTotal = reply.Incident, reply.Alerts.TotalCount
	if reply.Alerts.Data != nil {
		out.Alerts = reply.Alerts.Data
	}
	return nil, out, nil
}

// ---- alerts ------------------------------------------------------------

type alertsIn struct {
	Severity      string `json:"severity,omitempty"`
	CreatedAfter  string `json:"created_after,omitempty"`
	CreatedBefore string `json:"created_before,omitempty"`
	page
}

type alertsOut struct {
	Alerts  []alertSummary `json:"alerts"`
	Count   int            `json:"count"`
	Total   int            `json:"total"`
	HasMore bool           `json:"has_more"`
}

func (v *service) listAlerts(ctx context.Context, _ *mcp.CallToolRequest, in alertsIn) (*mcp.CallToolResult, alertsOut, error) {
	out := alertsOut{Alerts: []alertSummary{}}
	from, to, err := in.bounds()
	if err != nil {
		return nil, out, err
	}
	sev, err := enumFilter("severity", "in", "severity", in.Severity, severities)
	if err != nil {
		return nil, out, err
	}
	after, err := timeFilter("creation_time", "gte", "created_after", in.CreatedAfter)
	if err != nil {
		return nil, out, err
	}
	before, err := timeFilter("creation_time", "lte", "created_before", in.CreatedBefore)
	if err != nil {
		return nil, out, err
	}
	var reply struct {
		TotalCount int            `json:"total_count"`
		Alerts     []alertSummary `json:"alerts"`
	}
	data := map[string]any{
		"filters": collect(sev, after, before), "search_from": from, "search_to": to,
		"sort": map[string]string{"field": "creation_time", "keyword": "desc"},
	}
	if err := v.call(ctx, "list alerts", []string{"alerts", "get_alerts"}, data, &reply); err != nil {
		return nil, out, err
	}
	if reply.Alerts != nil {
		out.Alerts = reply.Alerts
	}
	out.Count, out.Total = len(out.Alerts), reply.TotalCount
	out.HasMore = from+out.Count < out.Total
	return nil, out, nil
}

// ---- endpoints ---------------------------------------------------------

type endpointsIn struct {
	EndpointID string `json:"endpoint_id,omitempty" jsonschema:"the id of one endpoint"`
	Hostname   string `json:"hostname,omitempty" jsonschema:"the exact host name"`
	Status     string `json:"status,omitempty"`
	Isolation  string `json:"isolation,omitempty"`
	page
}

type endpoint struct {
	EndpointID     string   `json:"endpoint_id"`
	Name           string   `json:"endpoint_name,omitempty"`
	Type           string   `json:"endpoint_type,omitempty"`
	Status         string   `json:"endpoint_status,omitempty"`
	OSType         string   `json:"os_type,omitempty"`
	IP             []string `json:"ip,omitempty"`
	LastSeen       int64    `json:"last_seen,omitempty"`
	IsolatedStatus string   `json:"is_isolated,omitempty"`
}

type endpointsOut struct {
	Endpoints []endpoint `json:"endpoints"`
	Count     int        `json:"count"`
	Total     int        `json:"total"`
	HasMore   bool       `json:"has_more"`
}

func (v *service) listEndpoints(ctx context.Context, _ *mcp.CallToolRequest, in endpointsIn) (*mcp.CallToolResult, endpointsOut, error) {
	out := endpointsOut{Endpoints: []endpoint{}}
	from, to, err := in.bounds()
	if err != nil {
		return nil, out, err
	}
	var fs []*filter
	if in.EndpointID != "" {
		if err := kit.CheckString("endpoint_id", in.EndpointID, 64, endpointFormat); err != nil {
			return nil, out, fmt.Errorf("check endpoint_id: %w", err)
		}
		fs = append(fs, &filter{Field: "endpoint_id_list", Operator: "in", Value: []string{in.EndpointID}})
	}
	if in.Hostname != "" {
		if err := kit.CheckString("hostname", in.Hostname, 255, hostFormat); err != nil {
			return nil, out, fmt.Errorf("check hostname: %w", err)
		}
		fs = append(fs, &filter{Field: "hostname", Operator: "in", Value: []string{in.Hostname}})
	}
	for _, e := range []struct {
		field, name, val string
		allowed          map[string]bool
	}{{"endpoint_status", "status", in.Status, endpointStatuses}, {"isolate", "isolation", in.Isolation, isolations}} {
		f, err := enumFilter(e.field, "in", e.name, e.val, e.allowed)
		if err != nil {
			return nil, out, err
		}
		fs = append(fs, f)
	}
	var reply struct {
		TotalCount int        `json:"total_count"`
		Endpoints  []endpoint `json:"endpoints"`
	}
	data := map[string]any{"filters": collect(fs...), "search_from": from, "search_to": to}
	if err := v.call(ctx, "list endpoints", []string{"endpoints", "get_endpoint"}, data, &reply); err != nil {
		return nil, out, err
	}
	if reply.Endpoints != nil {
		out.Endpoints = reply.Endpoints
	}
	out.Count, out.Total = len(out.Endpoints), reply.TotalCount
	out.HasMore = from+out.Count < out.Total
	return nil, out, nil
}

// ---- response actions --------------------------------------------------

type actionIn struct {
	EndpointID string `json:"endpoint_id" jsonschema:"the id of one endpoint. A list or a filter is not accepted."`
	IncidentID string `json:"incident_id,omitempty" jsonschema:"an incident id, digits. The action then shows in the timeline of the incident."`
}

type actionOut struct {
	EndpointID     string `json:"endpoint_id"`
	Action         string `json:"action"`
	ActionID       string `json:"action_id"`
	EndpointsCount string `json:"endpoints_count"`
}

func (v *service) isolate(ctx context.Context, _ *mcp.CallToolRequest, in actionIn) (*mcp.CallToolResult, actionOut, error) {
	return v.respond(ctx, "isolate", in)
}

func (v *service) unisolate(ctx context.Context, _ *mcp.CallToolRequest, in actionIn) (*mcp.CallToolResult, actionOut, error) {
	return v.respond(ctx, "unisolate", in)
}

// respond sends the isolate or the unisolate call for one endpoint. The call
// is not repeated, because a repeat after a server error could make a second
// action.
func (v *service) respond(ctx context.Context, op string, in actionIn) (*mcp.CallToolResult, actionOut, error) {
	out := actionOut{EndpointID: in.EndpointID, Action: op + "_endpoint"}
	if err := kit.CheckString("endpoint_id", in.EndpointID, 64, endpointFormat); err != nil {
		return nil, out, fmt.Errorf("check endpoint_id: %w", err)
	}
	data := map[string]any{"endpoint_id": in.EndpointID}
	if in.IncidentID != "" {
		if err := kit.CheckString("incident_id", in.IncidentID, 20, incidentFormat); err != nil {
			return nil, out, fmt.Errorf("check incident_id: %w", err)
		}
		data["incident_id"] = in.IncidentID
	}
	var reply struct {
		ActionID       any `json:"action_id"`
		EndpointsCount any `json:"endpoints_count"`
	}
	if err := v.send(ctx, op+" endpoint", []string{"endpoints", op}, data, &reply, false); err != nil {
		return nil, out, err
	}
	out.ActionID, out.EndpointsCount = fmt.Sprint(reply.ActionID), fmt.Sprint(reply.EndpointsCount)
	if reply.ActionID == nil || out.EndpointsCount != "1" {
		return nil, out, fmt.Errorf("%s endpoint: Cortex XDR did not confirm one endpoint (endpoints_count %s)", op, out.EndpointsCount)
	}
	return nil, out, nil
}
