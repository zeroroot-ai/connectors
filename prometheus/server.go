package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
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
	rawHealth := env.Optional("PROMETHEUS_HEALTH_QUERIES", "")
	if token != "" && (user != "" || pass != "") {
		return nil, errors.New("set PROMETHEUS_BEARER_TOKEN or PROMETHEUS_USERNAME with PROMETHEUS_PASSWORD, not both")
	}
	if (user == "") != (pass == "") {
		return nil, errors.New("PROMETHEUS_USERNAME and PROMETHEUS_PASSWORD go together")
	}
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	health, err := parseHealthQueries(rawHealth)
	if err != nil {
		return nil, err
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
	svc := &service{client: kit.NewClient(base, kit.ClientOptions{Auth: auth, Logger: log, Timeout: timeout}), now: time.Now, health: health}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	client *kit.Client
	now    func() time.Time
	// health maps the key of an application to the PromQL query that the
	// tenant states as the health of that application.
	health map[string]string
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
	kit.AddTool(s, log, &mcp.Tool{
		Name: "health_signal",
		Description: "Read the health signal of one application over a hold time. The tenant states one PromQL query for each " +
			"application in PROMETHEUS_HEALTH_QUERIES. The query is healthy while it returns a value other than 0, for example " +
			"a comparison with the bool modifier. The tool runs the query over the hold time that starts at since. " +
			"It returns known false when the tenant stated no query for the application. It returns done false while the hold " +
			"time still runs and the query never failed. It returns done true and held false when the value was 0 or NaN at one time, " +
			"or when the query returned no data or had gaps. It returns done true and held true when the query held " +
			"for the whole time. The result holds the value and the time of the last or the failed point. Read only.",
		Annotations: kit.ReadOnly("Prometheus health signal"),
	}, v.healthSignal)
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
		return nil, out, fmt.Errorf("%w: the range holds more than %d points. Use a larger step", kit.ErrInvalidInput, maxPoints)
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

// ---- health_signal ----

// The tool follows the range query of the Prometheus HTTP API:
// https://prometheus.io/docs/prometheus/latest/querying/api/#range-queries
// The input and the result follow internal/engine/fixloop/connector.go of
// zeroroot-ai/gibson (ToolHealthSignal).

const (
	maxHoldSeconds = 7 * 24 * 3600
	minHealthStep  = 15 * time.Second
	maxHealthSteps = 1000
	maxApplication = 256
)

// parseHealthQueries reads the JSON object of PROMETHEUS_HEALTH_QUERIES.
func parseHealthQueries(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, errors.New("PROMETHEUS_HEALTH_QUERIES must be a JSON object that maps an application to a query")
	}
	for app, q := range out {
		if kit.CheckString("application", app, maxApplication, nil) != nil || kit.CheckString("query", q, maxQueryLen, nil) != nil {
			return nil, errors.New("PROMETHEUS_HEALTH_QUERIES holds an empty or a too long application or query")
		}
	}
	return out, nil
}

type healthInput struct {
	Application string `json:"application" jsonschema:"the application key that deployment_state returned"`
	Since       string `json:"since" jsonschema:"start of the hold time, RFC 3339"`
	HoldSeconds int    `json:"hold_seconds" jsonschema:"how long the signal must hold, 0 to 604800 seconds"`
}

type healthOutput struct {
	Known  bool   `json:"known"`
	Done   bool   `json:"done"`
	Held   bool   `json:"held"`
	At     string `json:"at,omitempty"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type matrixResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		Result []struct {
			Values [][]any `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (v *service) healthSignal(ctx context.Context, _ *mcp.CallToolRequest, in healthInput) (*mcp.CallToolResult, healthOutput, error) {
	var out healthOutput
	if err := kit.CheckString("application", in.Application, maxApplication, nil); err != nil {
		return nil, out, err
	}
	if err := kit.CheckString("since", in.Since, 40, timeFormat); err != nil {
		return nil, out, err
	}
	since, err := time.Parse(time.RFC3339Nano, in.Since)
	if err != nil {
		return nil, out, fmt.Errorf("%w: since must be an RFC 3339 time", kit.ErrInvalidInput)
	}
	if err := kit.CheckRange("hold_seconds", in.HoldSeconds, 0, maxHoldSeconds); err != nil {
		return nil, out, err
	}
	query, ok := v.health[in.Application]
	if !ok {
		out.Reason = "the tenant stated no health query for this application"
		return nil, out, nil
	}
	out.Known = true
	hold := time.Duration(in.HoldSeconds) * time.Second
	end, now := since.Add(hold), v.now()
	if now.Before(since) {
		out.At, out.Reason = fixloop.Time(now), "the hold time has not started"
		return nil, out, nil
	}
	evalEnd := end
	if now.Before(end) {
		evalEnd = now
	}
	step := max(minHealthStep, (hold/maxHealthSteps).Round(time.Second)+time.Second)
	series, err := v.readSeries(ctx, query, since, evalEnd, step)
	if err != nil {
		return nil, out, err
	}
	return nil, judgeHealth(out, series, hold, step, now.Before(end), evalEnd), nil
}

type point struct {
	at    time.Time
	value float64
	text  string
}

func (v *service) readSeries(ctx context.Context, query string, start, end time.Time, step time.Duration) ([][]point, error) {
	form := url.Values{"query": {query}, "start": {start.UTC().Format(time.RFC3339Nano)}, "end": {end.UTC().Format(time.RFC3339Nano)},
		"step": {strconv.Itoa(int(step / time.Second))}}
	var resp matrixResponse
	// A query is a read, so a retry of the POST is safe.
	err := v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: "/api/v1/query_range", Form: form, Retry: true}, &resp)
	if err != nil {
		return nil, queryError(err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s: %s", resp.ErrorType, resp.Error)
	}
	var all [][]point
	for _, s := range resp.Data.Result {
		var pts []point
		for _, raw := range s.Values {
			if len(raw) != 2 {
				continue
			}
			ts, ok1 := raw[0].(float64)
			text, ok2 := raw[1].(string)
			val, err := strconv.ParseFloat(text, 64)
			if !ok1 || !ok2 || err != nil {
				return nil, errors.New("prometheus answered a sample that the tool cannot read")
			}
			pts = append(pts, point{at: time.Unix(int64(ts), int64((ts-math.Floor(ts))*1e9)), value: val, text: text})
		}
		all = append(all, pts)
	}
	return all, nil
}

// judgeHealth decides the result. A value of 0 or NaN is a failure, and the
// first failure ends the hold time at once. A hold time that ran to its end
// holds only when each series has samples for most of it.
func judgeHealth(out healthOutput, series [][]point, hold, step time.Duration, running bool, evalEnd time.Time) healthOutput {
	var last *point
	for _, pts := range series {
		for i := range pts {
			p := &pts[i]
			if p.value == 0 || math.IsNaN(p.value) {
				out.Done, out.Held, out.At, out.Value = true, false, fixloop.Time(p.at), p.text
				out.Reason = "the value of the health query was " + p.text
				return out
			}
			if last == nil || p.at.After(last.at) {
				last = p
			}
		}
	}
	if last != nil {
		out.At, out.Value = fixloop.Time(last.at), last.text
	}
	if running {
		out.Reason = "the hold time still runs"
		if last == nil {
			out.At = fixloop.Time(evalEnd)
		}
		return out
	}
	out.Done = true
	out.At = fixloop.Time(evalEnd)
	if len(series) == 0 {
		out.Reason = "the health query returned no data for the hold time"
		return out
	}
	expected := int(hold/step) + 1
	for _, pts := range series {
		if len(pts)*10 < expected*9 {
			out.Reason = "the health query had gaps in the hold time"
			return out
		}
	}
	out.Held = true
	return out
}
