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

// The tools follow the HTTP API of Prometheus:
// https://prometheus.io/docs/prometheus/latest/querying/api/
// (instant query, range query and /api/v1/alerts).

const (
	maxQueryLen    = 4096
	defaultLimit   = 100
	maxLimit       = 1000
	maxPoints      = 11000 // Prometheus refuses a range query with more points per series
	maxRange       = 31 * 24 * time.Hour
	maxAlertFilter = 128
)

var (
	// timeFormat is an RFC 3339 time or a Unix time in seconds.
	timeFormat = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T[0-9:.]+(Z|[+-]\d{2}:\d{2})|\d{1,10}(\.\d{1,3})?)$`)
	// durationFormat is a step: seconds as a number, or one unit of the
	// Prometheus duration syntax.
	durationFormat = regexp.MustCompile(`^(\d+(\.\d+)?|\d+(ms|s|m|h|d|w))$`)
	// alertName is the format of a label value that the alert filter accepts.
	alertName = regexp.MustCompile(`^[A-Za-z0-9_:.\-]+$`)
)

// build reads the configuration from the environment.
func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	base := env.URL("PROMETHEUS_BASE_URL")
	token := env.OptionalSecret("PROMETHEUS_BEARER_TOKEN")
	user := env.Optional("PROMETHEUS_USERNAME", "")
	pass := env.OptionalSecret("PROMETHEUS_PASSWORD")
	orgID := env.Optional("PROMETHEUS_ORG_ID", "")
	timeout := env.Duration("PROMETHEUS_TIMEOUT", 30*time.Second)
	if token != "" && (user != "" || pass != "") {
		return nil, errors.New("set PROMETHEUS_BEARER_TOKEN or PROMETHEUS_USERNAME with PROMETHEUS_PASSWORD, not both")
	}
	if (user == "") != (pass == "") {
		return nil, errors.New("PROMETHEUS_USERNAME and PROMETHEUS_PASSWORD go together")
	}
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	var auth kit.Authenticator
	switch {
	case token != "":
		auth = kit.HeaderAuth("Authorization", "Bearer ", token)
	case user != "":
		auth = kit.BasicAuth(user, pass)
	}
	if orgID != "" {
		// Mimir, Cortex and Thanos Receive select the tenant with this header.
		inner := auth
		auth = func(ctx context.Context, req *http.Request) error {
			req.Header.Set("X-Scope-OrgID", orgID)
			if inner != nil {
				return inner(ctx, req)
			}
			return nil
		}
	}
	svc := &service{client: kit.NewClient(base, kit.ClientOptions{Auth: auth, Logger: log, Timeout: timeout}), now: time.Now}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	client *kit.Client
	now    func() time.Time
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "query_instant",
		Description: "Run one PromQL instant query and return its result. Read only. " +
			"Use it to read the value of a health signal now, for example the error rate of one service.",
		Annotations: kit.ReadOnly("Prometheus instant query"),
	}, v.queryInstant)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "query_range",
		Description: "Run one PromQL range query and return the series over a time range. Read only. " +
			"The range is at most 31 days and at most 11000 points for each series.",
		Annotations: kit.ReadOnly("Prometheus range query"),
	}, v.queryRange)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_alerts",
		Description: "List the active alerts (firing and pending) that Prometheus holds. Read only. " +
			"An alert is an event, not a finding.",
		Annotations: kit.ReadOnly("Prometheus alerts"),
	}, v.listAlerts)
}

// ---- query_instant ----

type instantInput struct {
	Query string `json:"query" jsonschema:"PromQL expression, up to 4096 characters"`
	Time  string `json:"time,omitempty" jsonschema:"evaluation time, RFC 3339 or Unix seconds. The default is now."`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of series to return, 1 to 1000. The default is 100."`
}

type queryOutput struct {
	ResultType string   `json:"resultType"`
	Result     any      `json:"result"`
	Series     int      `json:"series"`
	Truncated  bool     `json:"truncated"`
	Warnings   []string `json:"warnings,omitempty"`
}

type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     any    `json:"result"`
	} `json:"data"`
	Warnings []string `json:"warnings"`
}

func (v *service) queryInstant(ctx context.Context, _ *mcp.CallToolRequest, in instantInput) (*mcp.CallToolResult, queryOutput, error) {
	var out queryOutput
	if err := kit.CheckString("query", in.Query, maxQueryLen, nil); err != nil {
		return nil, out, err
	}
	limit, err := checkLimit(in.Limit)
	if err != nil {
		return nil, out, err
	}
	form := url.Values{"query": {in.Query}, "limit": {strconv.Itoa(limit)}}
	if in.Time != "" {
		if err := kit.CheckString("time", in.Time, 40, timeFormat); err != nil {
			return nil, out, err
		}
		form.Set("time", in.Time)
	}
	return v.runQuery(ctx, "/api/v1/query", form, limit)
}

// ---- query_range ----

type rangeInput struct {
	Query string `json:"query" jsonschema:"PromQL expression, up to 4096 characters"`
	Start string `json:"start" jsonschema:"start time, RFC 3339 or Unix seconds"`
	End   string `json:"end" jsonschema:"end time, RFC 3339 or Unix seconds"`
	Step  string `json:"step" jsonschema:"resolution: seconds as a number, or a duration such as 30s, 1m, 1h"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of series to return, 1 to 1000. The default is 100."`
}

func (v *service) queryRange(ctx context.Context, _ *mcp.CallToolRequest, in rangeInput) (*mcp.CallToolResult, queryOutput, error) {
	var out queryOutput
	if err := kit.CheckString("query", in.Query, maxQueryLen, nil); err != nil {
		return nil, out, err
	}
	limit, err := checkLimit(in.Limit)
	if err != nil {
		return nil, out, err
	}
	for field, val := range map[string]string{"start": in.Start, "end": in.End} {
		if err := kit.CheckString(field, val, 40, timeFormat); err != nil {
			return nil, out, err
		}
	}
	if err := kit.CheckString("step", in.Step, 16, durationFormat); err != nil {
		return nil, out, err
	}
	start, _ := parseTime(in.Start)
	end, _ := parseTime(in.End)
	step, _ := parseStep(in.Step)
	switch {
	case !end.After(start):
		return nil, out, fmt.Errorf("%w: end must be after start", kit.ErrInvalidInput)
	case end.Sub(start) > maxRange:
		return nil, out, fmt.Errorf("%w: the range is longer than 31 days", kit.ErrInvalidInput)
	case step < time.Second/1000:
		return nil, out, fmt.Errorf("%w: step is too small", kit.ErrInvalidInput)
	case float64(end.Sub(start))/float64(step) > maxPoints:
		return nil, out, fmt.Errorf("%w: the range holds more than %d points. Use a larger step.", kit.ErrInvalidInput, maxPoints)
	}
	form := url.Values{"query": {in.Query}, "start": {in.Start}, "end": {in.End}, "step": {in.Step},
		"limit": {strconv.Itoa(limit)}}
	return v.runQuery(ctx, "/api/v1/query_range", form, limit)
}

func (v *service) runQuery(ctx context.Context, path string, form url.Values, limit int) (*mcp.CallToolResult, queryOutput, error) {
	var out queryOutput
	var resp promResponse
	// A query is a read, so a retry of the POST is safe.
	err := v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: path, Form: form, Retry: true}, &resp)
	if err != nil {
		return nil, out, queryError(err)
	}
	if resp.Status != "success" {
		return nil, out, fmt.Errorf("prometheus query failed: %s: %s", resp.ErrorType, resp.Error)
	}
	out.ResultType, out.Result, out.Warnings = resp.Data.ResultType, resp.Data.Result, resp.Warnings
	if list, ok := resp.Data.Result.([]any); ok {
		out.Series = len(list)
		if len(list) > limit {
			out.Result, out.Truncated = list[:limit], true
		}
	}
	return nil, out, nil
}

// queryError returns the error of Prometheus for a bad query, so the model can
// repair the query.
func queryError(err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusUnprocessableEntity) {
		var pr promResponse
		if kit.DecodeErrorBody(ae, &pr) == nil && pr.Error != "" {
			return fmt.Errorf("prometheus rejected the query: %s: %s", pr.ErrorType, pr.Error)
		}
	}
	return fmt.Errorf("prometheus query: %w", err)
}

// ---- list_alerts ----

type alertsInput struct {
	State     string `json:"state,omitempty" jsonschema:"filter: firing or pending. The default is both."`
	AlertName string `json:"alertname,omitempty" jsonschema:"filter: exact value of the alertname label"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum number of alerts, 1 to 1000. The default is 100."`
}

type alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	State       string            `json:"state"`
	ActiveAt    string            `json:"activeAt,omitempty"`
	Value       string            `json:"value,omitempty"`
}

type alertsOutput struct {
	Alerts    []alert `json:"alerts"`
	Total     int     `json:"total"`
	Truncated bool    `json:"truncated"`
}

func (v *service) listAlerts(ctx context.Context, _ *mcp.CallToolRequest, in alertsInput) (*mcp.CallToolResult, alertsOutput, error) {
	out := alertsOutput{Alerts: []alert{}}
	limit, err := checkLimit(in.Limit)
	if err != nil {
		return nil, out, err
	}
	switch in.State {
	case "", "firing", "pending":
	default:
		return nil, out, fmt.Errorf("%w: state must be firing or pending", kit.ErrInvalidInput)
	}
	if in.AlertName != "" {
		if err := kit.CheckString("alertname", in.AlertName, maxAlertFilter, alertName); err != nil {
			return nil, out, err
		}
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []alert `json:"alerts"`
		} `json:"data"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: "/api/v1/alerts"}, &resp); err != nil {
		return nil, out, fmt.Errorf("prometheus alerts: %w", err)
	}
	for _, a := range resp.Data.Alerts {
		if (in.State != "" && a.State != in.State) || (in.AlertName != "" && a.Labels["alertname"] != in.AlertName) {
			continue
		}
		out.Total++
		if len(out.Alerts) < limit {
			out.Alerts = append(out.Alerts, a)
		} else {
			out.Truncated = true
		}
	}
	return nil, out, nil
}

// ---- helpers ----

func checkLimit(n int) (int, error) {
	if n == 0 {
		return defaultLimit, nil
	}
	if err := kit.CheckRange("limit", n, 1, maxLimit); err != nil {
		return 0, err
	}
	return n, nil
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time: %w", err)
	}
	return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)), nil
}

func parseStep(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	n := strings.TrimRight(s, "smhdw")
	unit := s[len(n):]
	if strings.HasSuffix(s, "ms") {
		n, unit = strings.TrimSuffix(s, "ms"), "ms"
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return 0, fmt.Errorf("parse step: %w", err)
	}
	mult := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[unit]
	return time.Duration(f * float64(mult)), nil
}
