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
)

// The tools use the InsightIDR API v2. The OpenAPI spec is
// https://help.rapid7.com/insightidr/en-us/api/v2/insightidr-api-v2.json and the
// reference is https://help.rapid7.com/insightidr/en-us/api/v2/docs.html
//   - GET /idr/v2/investigations  (query index, size up to 100, statuses, priorities,
//     sources, start_time, end_time; answer {"data":[...],"metadata":{index,size,total_pages,total_data}})
//   - GET /idr/v2/investigations/{id}   (the id or the rrn)
//   - GET /idr/v2/investigations/{identifier}/alerts  (query index, size; answer {"data":[...],"metadata":{...}})
// The host is https://{region}.api.insight.rapid7.com and the header is X-Api-Key
// (https://docs.rapid7.com/insight/product-apis/ and
// https://docs.rapid7.com/insight/api-overview/).

var (
	regionFormat = regexp.MustCompile(`^[a-z]{2}[a-z0-9]{0,2}$`)
	idFormat     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,199}$`)
)

const (
	defaultSize = 20
	maxSize     = 100
	maxIndex    = 10000
)

var (
	statuses   = map[string]bool{"OPEN": true, "INVESTIGATING": true, "CLOSED": true}
	priorities = map[string]bool{"CRITICAL": true, "HIGH": true, "MEDIUM": true, "LOW": true, "UNSPECIFIED": true}
	sources    = map[string]bool{"MANUAL": true, "HUNT": true, "ALERT": true}
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	key := env.Secret("INSIGHTIDR_API_KEY")
	region := env.Optional("INSIGHTIDR_REGION", "us")
	override := env.Optional("INSIGHTIDR_URL", "")
	timeout := env.Duration("INSIGHTIDR_TIMEOUT", 30*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if !regionFormat.MatchString(region) {
		return nil, errors.New("INSIGHTIDR_REGION has the wrong format. Use a region code such as us, us2, us3, eu, ca, au, ap, me1 or aps2")
	}
	raw := "https://" + region + ".api.insight.rapid7.com"
	if override != "" {
		raw = override
	}
	base, err := kit.ParseBaseURL(raw)
	if err != nil {
		return nil, errors.New("INSIGHTIDR_URL is not a valid base URL")
	}
	if base.Scheme != "https" {
		return nil, errors.New("INSIGHTIDR_URL must use https")
	}
	client := kit.NewClient(base, kit.ClientOptions{
		Auth:   kit.HeaderAuth("X-Api-Key", "", key),
		Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_investigations",
		Description: "List InsightIDR investigations: title, status, priority, source, disposition and times. Read only. All inputs are optional. " +
			"status is OPEN, INVESTIGATING or CLOSED. priority is CRITICAL, HIGH, MEDIUM, LOW or UNSPECIFIED. source is MANUAL, HUNT or ALERT. " +
			"start_time and end_time are RFC 3339 times. InsightIDR uses 28 days before now as the default start. " +
			"The id of an investigation is its rrn.",
		Annotations: kit.ReadOnly("List InsightIDR investigations"),
	}, v.listInvestigations)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "get_investigation",
		Description: "Read one InsightIDR investigation by its rrn or its id. Read only.",
		Annotations: kit.ReadOnly("Get InsightIDR investigation"),
	}, v.getInvestigation)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "list_investigation_alerts",
		Description: "List the alerts of one InsightIDR investigation: title, type, source, detection rule and the times of the first and the latest event. Read only.",
		Annotations: kit.ReadOnly("List InsightIDR investigation alerts"),
	}, v.listAlerts)
}

type pageIn struct {
	Index int `json:"index,omitempty" jsonschema:"the page, starting at 0. The default is 0."`
	Size  int `json:"size,omitempty" jsonschema:"records on a page, 1 to 100. The default is 20."`
}

func (p pageIn) query(q url.Values) error {
	size := p.Size
	if size == 0 {
		size = defaultSize
	}
	if err := kit.CheckRange("size", size, 1, maxSize); err != nil {
		return fmt.Errorf("check size: %w", err)
	}
	if err := kit.CheckRange("index", p.Index, 0, maxIndex); err != nil {
		return fmt.Errorf("check index: %w", err)
	}
	q.Set("index", strconv.Itoa(p.Index))
	q.Set("size", strconv.Itoa(size))
	return nil
}

type metadata struct {
	Index      int `json:"index"`
	Size       int `json:"size"`
	TotalPages int `json:"total_pages"`
	TotalData  int `json:"total_data"`
}

type pageOut struct {
	Index        int `json:"index"`
	TotalPages   int `json:"total_pages"`
	TotalRecords int `json:"total_records"`
}

func (m metadata) out() pageOut {
	return pageOut{Index: m.Index, TotalPages: m.TotalPages, TotalRecords: m.TotalData}
}

type investigation struct {
	RRN             any    `json:"rrn"`
	Title           string `json:"title"`
	Source          string `json:"source"`
	Status          string `json:"status"`
	Priority        string `json:"priority"`
	Disposition     string `json:"disposition"`
	Assignee        any    `json:"assignee"`
	CreatedTime     string `json:"created_time"`
	LastAccessed    string `json:"last_accessed"`
	FirstAlertTime  string `json:"first_alert_time"`
	LatestAlertTime string `json:"latest_alert_time"`
}

type investigationOut struct {
	ID              string `json:"investigation_id,omitempty"`
	Title           string `json:"title"`
	Source          string `json:"source,omitempty"`
	Status          string `json:"status"`
	Priority        string `json:"priority,omitempty"`
	Disposition     string `json:"disposition,omitempty"`
	Assignee        string `json:"assignee,omitempty"`
	CreatedTime     string `json:"created_time,omitempty"`
	LastAccessed    string `json:"last_accessed,omitempty"`
	FirstAlertTime  string `json:"first_alert_time,omitempty"`
	LatestAlertTime string `json:"latest_alert_time,omitempty"`
}

// assigneeText gives the e-mail address of an assignee. The spec types the
// assignee as text or null, and the shape of an object is not stated. The tool
// reads a text, or the fields email and name of an object.
func assigneeText(a any) string {
	switch x := a.(type) {
	case string:
		return x
	case map[string]any:
		for _, k := range []string{"email", "name"} {
			if s, ok := x[k].(string); ok {
				return s
			}
		}
	}
	return ""
}

// rrnText gives the text of an rrn. The spec types the rrn as an object with
// partition, service, regionCode, organizationId, resourceTypes and resource,
// and its examples are text such as rrn:cba:::detection-rule:FX11KBBSCK20. The
// tool takes a text as it is, and joins the parts of an object in that order.
func rrnText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		str := func(k string) string { s, _ := x[k].(string); return s }
		var types []string
		if arr, ok := x["resourceTypes"].([]any); ok {
			for _, t := range arr {
				if s, ok := t.(string); ok {
					types = append(types, s)
				}
			}
		}
		return "rrn:" + strings.Join([]string{str("partition"), str("service"), str("regionCode"), str("organizationId"), strings.Join(types, ":"), str("resource")}, ":")
	}
	return ""
}

func (i investigation) out(id string) investigationOut {
	if id == "" {
		id = rrnText(i.RRN)
	}
	return investigationOut{
		ID: id, Title: i.Title, Source: i.Source, Status: i.Status, Priority: i.Priority, Disposition: i.Disposition,
		Assignee: assigneeText(i.Assignee), CreatedTime: i.CreatedTime, LastAccessed: i.LastAccessed,
		FirstAlertTime: i.FirstAlertTime, LatestAlertTime: i.LatestAlertTime,
	}
}

type listInvestigationsIn struct {
	Status    string `json:"status,omitempty"`
	Priority  string `json:"priority,omitempty"`
	Source    string `json:"source,omitempty"`
	StartTime string `json:"start_time,omitempty"`
	EndTime   string `json:"end_time,omitempty"`
	pageIn
}

type listInvestigationsOut struct {
	Investigations []investigationOut `json:"investigations"`
	pageOut
}

func (v *service) listInvestigations(ctx context.Context, _ *mcp.CallToolRequest, in listInvestigationsIn) (*mcp.CallToolResult, listInvestigationsOut, error) {
	out := listInvestigationsOut{Investigations: []investigationOut{}}
	q := url.Values{}
	for _, e := range []struct {
		field, key, val string
		allowed         map[string]bool
	}{{"status", "statuses", in.Status, statuses}, {"priority", "priorities", in.Priority, priorities}, {"source", "sources", in.Source, sources}} {
		if e.val == "" {
			continue
		}
		if !e.allowed[e.val] {
			return nil, out, fmt.Errorf("%w: %s has a value that InsightIDR does not know", kit.ErrInvalidInput, e.field)
		}
		q.Set(e.key, e.val)
	}
	for _, t := range []struct{ field, key, val string }{{"start_time", "start_time", in.StartTime}, {"end_time", "end_time", in.EndTime}} {
		if t.val == "" {
			continue
		}
		if err := kit.CheckString(t.field, t.val, 40, nil); err != nil {
			return nil, out, fmt.Errorf("check %s: %w", t.field, err)
		}
		if _, err := time.Parse(time.RFC3339, t.val); err != nil {
			return nil, out, fmt.Errorf("%w: %s is not an RFC 3339 time", kit.ErrInvalidInput, t.field)
		}
		q.Set(t.key, t.val)
	}
	if err := in.query(q); err != nil {
		return nil, out, err
	}
	var resp struct {
		Data     []investigation `json:"data"`
		Metadata metadata        `json:"metadata"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("idr", "v2", "investigations"), Query: q}, &resp); err != nil {
		return nil, out, fmt.Errorf("list investigations: %w", err)
	}
	for _, i := range resp.Data {
		out.Investigations = append(out.Investigations, i.out(""))
	}
	out.pageOut = resp.Metadata.out()
	return nil, out, nil
}

type idIn struct {
	InvestigationID string `json:"investigation_id" jsonschema:"the rrn of the investigation, for example rrn:investigation:us1:...:investigation:6A74T2A4, or its id"`
}

func checkID(id string) error {
	if err := kit.CheckString("investigation_id", id, 200, idFormat); err != nil {
		return fmt.Errorf("check investigation_id: %w", err)
	}
	return nil
}

func (v *service) getInvestigation(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, investigationOut, error) {
	var out investigationOut
	if err := checkID(in.InvestigationID); err != nil {
		return nil, out, err
	}
	var i investigation
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("idr", "v2", "investigations", in.InvestigationID)}, &i)
	if err != nil {
		return nil, out, fmt.Errorf("get investigation: %w", err)
	}
	if i.Title == "" && i.Status == "" {
		return nil, out, errors.New("get investigation: InsightIDR did not return an investigation")
	}
	return nil, i.out(in.InvestigationID), nil
}

type alertsIn struct {
	idIn
	pageIn
}

type alert struct {
	ID                   string `json:"alert_id"`
	Title                string `json:"title"`
	AlertType            string `json:"alert_type,omitempty"`
	AlertTypeDescription string `json:"alert_type_description,omitempty"`
	AlertSource          string `json:"alert_source,omitempty"`
	DetectionRuleName    string `json:"detection_rule_name,omitempty"`
	DetectionRuleRRN     string `json:"detection_rule_rrn,omitempty"`
	CreatedTime          string `json:"created_time,omitempty"`
	FirstEventTime       string `json:"first_event_time,omitempty"`
	LatestEventTime      string `json:"latest_event_time,omitempty"`
}

type alertsOut struct {
	Alerts []alert `json:"alerts"`
	pageOut
}

func (v *service) listAlerts(ctx context.Context, _ *mcp.CallToolRequest, in alertsIn) (*mcp.CallToolResult, alertsOut, error) {
	out := alertsOut{Alerts: []alert{}}
	if err := checkID(in.InvestigationID); err != nil {
		return nil, out, err
	}
	q := url.Values{}
	if err := in.query(q); err != nil {
		return nil, out, err
	}
	var resp struct {
		Data []struct {
			ID                   string `json:"id"`
			Title                string `json:"title"`
			AlertType            string `json:"alertType"`
			AlertTypeDescription string `json:"alertTypeDescription"`
			AlertSource          string `json:"alertSource"`
			// The spec types this field as an object {ruleName, ruleRrn}.
			DetectionRule struct {
				RuleName string `json:"ruleName"`
				RuleRRN  any    `json:"ruleRrn"`
			} `json:"detectionRuleRrn"`
			CreatedTime     string `json:"createdTime"`
			FirstEventTime  string `json:"firstEventTime"`
			LatestEventTime string `json:"latestEventTime"`
		} `json:"data"`
		Metadata metadata `json:"metadata"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("idr", "v2", "investigations", in.InvestigationID, "alerts"), Query: q,
	}, &resp)
	if err != nil {
		return nil, out, fmt.Errorf("list investigation alerts: %w", err)
	}
	for _, a := range resp.Data {
		out.Alerts = append(out.Alerts, alert{
			ID: a.ID, Title: a.Title, AlertType: a.AlertType, AlertTypeDescription: a.AlertTypeDescription, AlertSource: a.AlertSource,
			DetectionRuleName: a.DetectionRule.RuleName, DetectionRuleRRN: rrnText(a.DetectionRule.RuleRRN), CreatedTime: a.CreatedTime, FirstEventTime: a.FirstEventTime, LatestEventTime: a.LatestEventTime,
		})
	}
	out.pageOut = resp.Metadata.out()
	return nil, out, nil
}
