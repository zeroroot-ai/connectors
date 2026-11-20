package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows https://prometheus.io/docs/prometheus/latest/querying/api/
// (instant query, range query and /api/v1/alerts, with the error envelope).
type mock struct {
	calls []string
	form  map[string]string
	auth  string
	org   string
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.auth, m.org = r.Header.Get("Authorization"), r.Header.Get("X-Scope-OrgID")
	_ = r.ParseForm()
	m.form = map[string]string{}
	for k := range r.PostForm {
		m.form[k] = r.PostForm.Get(k)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/prom/api/v1/query":
		if strings.Contains(r.PostForm.Get("query"), "((") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"1:3: parse error: unexpected \"(\""}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"__name__":"up","job":"api","instance":"a:9090"},"value":[1435781451.781,"1"]},` +
			`{"metric":{"__name__":"up","job":"api","instance":"b:9090"},"value":[1435781451.781,"0"]}]},` +
			`"warnings":["w1"]}`))
	case "/prom/api/v1/query_range":
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
			`{"metric":{"job":"api"},"values":[[1435781430.781,"1"],[1435781445.781,"1"]]}]}}`))
	case "/prom/api/v1/alerts":
		_, _ = w.Write([]byte(`{"status":"success","data":{"alerts":[` +
			`{"labels":{"alertname":"HighErrors","severity":"page"},"annotations":{"summary":"5xx"},"state":"firing","activeAt":"2026-11-20T10:00:00Z","value":"1e+00"},` +
			`{"labels":{"alertname":"SlowDisk"},"annotations":{},"state":"pending","activeAt":"2026-11-20T10:05:00Z","value":"2e+00"}]}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T, extra map[string]string) (srv *mock, call func(string, map[string]any) (bool, string)) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	cfg := map[string]string{"PROMETHEUS_BASE_URL": ts.URL + "/prom"}
	for k, v := range extra {
		cfg[k] = v
	}
	register, err := build(kit.NewEnvFrom(cfg), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}
}

func TestQueryInstant(t *testing.T) {
	m, call := setup(t, map[string]string{"PROMETHEUS_BEARER_TOKEN": "tok", "PROMETHEUS_ORG_ID": "tenant-a"})
	isErr, text := call("query_instant", map[string]any{"query": `up{job="api"}`, "time": "1435781451.781", "limit": 1})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	if m.form["query"] != `up{job="api"}` || m.form["time"] != "1435781451.781" || m.form["limit"] != "1" {
		t.Errorf("form = %v", m.form)
	}
	if m.auth != "Bearer tok" || m.org != "tenant-a" {
		t.Errorf("auth=%q org=%q", m.auth, m.org)
	}
	for _, want := range []string{`"resultType":"vector"`, `"series":2`, `"truncated":true`, `"w1"`} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "b:9090") {
		t.Errorf("limit 1 did not cut the series: %s", text)
	}
}

func TestQueryInstantBadQueryShowsVendorError(t *testing.T) {
	_, call := setup(t, nil)
	isErr, text := call("query_instant", map[string]any{"query": "sum(("})
	if !isErr || !strings.Contains(text, "bad_data") || !strings.Contains(text, "parse error") {
		t.Errorf("isErr=%v text=%s", isErr, text)
	}
}

func TestQueryRange(t *testing.T) {
	m, call := setup(t, nil)
	isErr, text := call("query_range", map[string]any{
		"query": "up", "start": "2026-11-20T10:00:00Z", "end": "2026-11-20T11:00:00Z", "step": "15s"})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	if m.calls[0] != "POST /prom/api/v1/query_range" || m.form["step"] != "15s" || m.form["start"] != "2026-11-20T10:00:00Z" {
		t.Errorf("calls=%v form=%v", m.calls, m.form)
	}
	if !strings.Contains(text, `"resultType":"matrix"`) || !strings.Contains(text, "1435781445.781") {
		t.Errorf("text = %s", text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call := setup(t, nil)
	good := map[string]any{"query": "up", "start": "1000", "end": "2000", "step": "10s"}
	with := func(k string, v any) map[string]any {
		c := map[string]any{}
		for a, b := range good {
			c[a] = b
		}
		c[k] = v
		return c
	}
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"empty query", "query_instant", map[string]any{"query": ""}},
		{"long query", "query_instant", map[string]any{"query": strings.Repeat("a", 4097)}},
		{"control char", "query_instant", map[string]any{"query": "up\x00"}},
		{"bad time", "query_instant", map[string]any{"query": "up", "time": "yesterday; ls"}},
		{"limit high", "query_instant", map[string]any{"query": "up", "limit": 5000}},
		{"bad step", "query_range", with("step", "1x")},
		{"end before start", "query_range", with("end", "500")},
		{"too long range", "query_range", with("end", "99999999")},
		{"too many points", "query_range", with("step", "0.01")},
		{"bad state", "list_alerts", map[string]any{"state": "inactive"}},
		{"bad alertname", "list_alerts", map[string]any{"alertname": "a b;c"}},
	}
	for _, c := range cases {
		m.calls = nil
		isErr, text := call(c.tool, c.args)
		if !isErr {
			t.Errorf("%s: accepted: %s", c.name, text)
		}
		if len(m.calls) != 0 {
			t.Errorf("%s: reached the vendor: %v", c.name, m.calls)
		}
	}
}

func TestListAlerts(t *testing.T) {
	_, call := setup(t, nil)
	isErr, text := call("list_alerts", map[string]any{"state": "firing"})
	if isErr || !strings.Contains(text, "HighErrors") || strings.Contains(text, "SlowDisk") {
		t.Errorf("state filter: %v %s", isErr, text)
	}
	_, text = call("list_alerts", map[string]any{"alertname": "SlowDisk"})
	if !strings.Contains(text, "SlowDisk") || strings.Contains(text, "HighErrors") {
		t.Errorf("name filter: %s", text)
	}
	_, text = call("list_alerts", map[string]any{"limit": 1})
	if !strings.Contains(text, `"total":2`) || !strings.Contains(text, `"truncated":true`) {
		t.Errorf("limit: %s", text)
	}
}

func TestAllToolsReadOnly(t *testing.T) {
	m := &mock{}
	ts := httptest.NewServer(m)
	defer ts.Close()
	register, err := build(kit.NewEnvFrom(map[string]string{"PROMETHEUS_BASE_URL": ts.URL}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tools := kittest.Tools(t, kittest.Connect(t, register))
	if len(tools) != 4 {
		t.Fatalf("tools = %d, want 4", len(tools))
	}
	for name, tool := range tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be read only", name)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for name, cfg := range map[string]map[string]string{
		"no url":      {},
		"bad url":     {"PROMETHEUS_BASE_URL": "file:///etc/passwd"},
		"both auth":   {"PROMETHEUS_BASE_URL": "http://p", "PROMETHEUS_BEARER_TOKEN": "t", "PROMETHEUS_USERNAME": "u", "PROMETHEUS_PASSWORD": "p"},
		"half basic":  {"PROMETHEUS_BASE_URL": "http://p", "PROMETHEUS_USERNAME": "u"},
		"bad timeout": {"PROMETHEUS_BASE_URL": "http://p", "PROMETHEUS_TIMEOUT": "soon"},
	} {
		if _, err := build(kit.NewEnvFrom(cfg), slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// ---- health_signal ----
//
// The mock follows the range query of
// https://prometheus.io/docs/prometheus/latest/querying/api/#range-queries.

const healthSince = "2026-11-20T10:00:00Z"

// series builds the matrix result for one series: n samples, one every step
// seconds from the start, all with the value v. A sample index in zeros has
// the value 0.
func series(start, step, n int, v string, zeros ...int) string {
	var b strings.Builder
	b.WriteString(`{"metric":{"service":"api"},"values":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		val := v
		for _, z := range zeros {
			if z == i {
				val = "0"
			}
		}
		b.WriteString(`[` + strconv.Itoa(start+i*step) + `,"` + val + `"]`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func matrix(parts ...string) string {
	return `{"status":"success","data":{"resultType":"matrix","result":[` + strings.Join(parts, ",") + `]}}`
}

func healthSetup(t *testing.T, now string, queries map[string]string, body string) (calls *int, call func(map[string]any) (isErr bool, text string, out map[string]any)) {
	t.Helper()
	calls = new(int)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/query_range" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	base, err := kit.ParseBaseURL(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	nowT, err := time.Parse(time.RFC3339, now)
	if err != nil {
		t.Fatal(err)
	}
	svc := &service{client: kit.NewClient(base, kit.ClientOptions{}), now: func() time.Time { return nowT }, health: queries}
	cs := kittest.Connect(t, func(s *mcp.Server) { svc.register(s, slog.New(slog.DiscardHandler)) })
	return calls, func(args map[string]any) (bool, string, map[string]any) {
		res := kittest.Call(t, cs, "health_signal", args)
		var out map[string]any
		if !res.IsError {
			kittest.Structured(t, res, &out)
		}
		return res.IsError, kittest.Text(t, res), out
	}
}

var healthQueries = map[string]string{"acme/shop": `(sum(rate(errors[1m])) / sum(rate(requests[1m])) < bool 0.01)`}

func healthArgs(app string, hold int) map[string]any {
	return map[string]any{"application": app, "since": healthSince, "hold_seconds": hold}
}

func TestHealthSignalHeld(t *testing.T) {
	// A hold time of 10 minutes at a 15 second step is 41 samples.
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	_, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix(series(start, 15, 41, "1")))
	isErr, text, out := call(healthArgs("acme/shop", 600))
	if isErr {
		t.Fatal(text)
	}
	if out["known"] != true || out["done"] != true || out["held"] != true || out["at"] != "2026-11-20T10:10:00Z" || out["value"] != "1" {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalSendsTheTenantQuery(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	body := matrix(series(start, 15, 41, "1"))
	var seen map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen = map[string]string{}
		for k := range r.PostForm {
			seen[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	base, _ := kit.ParseBaseURL(ts.URL)
	now, _ := time.Parse(time.RFC3339, "2026-11-20T10:30:00Z")
	svc := &service{client: kit.NewClient(base, kit.ClientOptions{}), now: func() time.Time { return now }, health: healthQueries}
	cs := kittest.Connect(t, func(s *mcp.Server) { svc.register(s, slog.New(slog.DiscardHandler)) })
	kittest.Call(t, cs, "health_signal", healthArgs("acme/shop", 600))
	if seen["query"] != healthQueries["acme/shop"] || seen["start"] != "2026-11-20T10:00:00Z" ||
		seen["end"] != "2026-11-20T10:10:00Z" || seen["step"] != "15" {
		t.Errorf("form = %v", seen)
	}
}

func TestHealthSignalFailedAtOnce(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	// The hold time still runs at 10:05, and one sample at 10:02:30 is 0.
	_, call := healthSetup(t, "2026-11-20T10:05:00Z", healthQueries, matrix(series(start, 15, 21, "1", 10)))
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["known"] != true || out["done"] != true || out["held"] != false || out["at"] != "2026-11-20T10:02:30Z" || out["value"] != "0" {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalNaNFails(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	body := matrix(`{"metric":{},"values":[[` + strconv.Itoa(start) + `,"1"],[` + strconv.Itoa(start+15) + `,"NaN"]]}`)
	_, call := healthSetup(t, "2026-11-20T10:05:00Z", healthQueries, body)
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["done"] != true || out["held"] != false || out["value"] != "NaN" {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalStillRunning(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	_, call := healthSetup(t, "2026-11-20T10:05:00Z", healthQueries, matrix(series(start, 15, 21, "1")))
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["known"] != true || out["done"] != false || out["held"] != false || out["at"] != "2026-11-20T10:05:00Z" {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalNoData(t *testing.T) {
	_, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix())
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["done"] != true || out["held"] != false || !strings.Contains(out["reason"].(string), "no data") {
		t.Errorf("out = %v", out)
	}
	// No data while the hold time runs is not a failure yet.
	_, call = healthSetup(t, "2026-11-20T10:05:00Z", healthQueries, matrix())
	_, _, out = call(healthArgs("acme/shop", 600))
	if out["done"] != false {
		t.Errorf("running: %v", out)
	}
}

func TestHealthSignalGaps(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	// 30 of 41 samples: the service stopped to report for a part of the time.
	_, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix(series(start, 15, 30, "1")))
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["done"] != true || out["held"] != false || !strings.Contains(out["reason"].(string), "gaps") {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalEverySeriesMustHold(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	_, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix(series(start, 15, 41, "1"), series(start, 15, 41, "1", 40)))
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["held"] != false || out["value"] != "0" {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalUnknownApplication(t *testing.T) {
	calls, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix())
	_, _, out := call(healthArgs("other/app", 600))
	if out["known"] != false || out["done"] != false || out["held"] != false {
		t.Errorf("out = %v", out)
	}
	if *calls != 0 {
		t.Errorf("a query ran for an unknown application")
	}
	_, call = healthSetup(t, "2026-11-20T10:30:00Z", nil, matrix())
	_, _, out = call(healthArgs("acme/shop", 600))
	if out["known"] != false {
		t.Errorf("no queries set: %v", out)
	}
}

func TestHealthSignalBeforeTheHoldTimeStarts(t *testing.T) {
	_, call := healthSetup(t, "2026-11-20T09:59:00Z", healthQueries, matrix())
	_, _, out := call(healthArgs("acme/shop", 600))
	if out["known"] != true || out["done"] != false || out["held"] != false {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalZeroHold(t *testing.T) {
	start := int(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix())
	_, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix(series(start, 15, 1, "1")))
	_, _, out := call(healthArgs("acme/shop", 0))
	if out["done"] != true || out["held"] != true {
		t.Errorf("out = %v", out)
	}
}

func TestHealthSignalInputValidation(t *testing.T) {
	calls, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, matrix())
	for name, args := range map[string]map[string]any{
		"empty application": {"application": "", "since": healthSince, "hold_seconds": 60},
		"bad since":         {"application": "acme/shop", "since": "yesterday", "hold_seconds": 60},
		"negative hold":     {"application": "acme/shop", "since": healthSince, "hold_seconds": -1},
		"huge hold":         {"application": "acme/shop", "since": healthSince, "hold_seconds": 99999999},
		"control char":      {"application": "acme\x00", "since": healthSince, "hold_seconds": 60},
	} {
		if isErr, _, _ := call(args); !isErr {
			t.Errorf("%s accepted", name)
		}
	}
	if *calls != 0 {
		t.Errorf("reached Prometheus")
	}
}

func TestHealthSignalVendorError(t *testing.T) {
	_, call := healthSetup(t, "2026-11-20T10:30:00Z", healthQueries, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	isErr, text, _ := call(healthArgs("acme/shop", 600))
	if !isErr || !strings.Contains(text, "bad_data") {
		t.Errorf("isErr=%v text=%s", isErr, text)
	}
}

func TestHealthQueriesConfig(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	for name, raw := range map[string]string{
		"not json":    `acme=up`,
		"not object":  `["up"]`,
		"empty query": `{"acme/shop":""}`,
		"empty app":   `{"":"up"}`,
	} {
		_, err := build(kit.NewEnvFrom(map[string]string{"PROMETHEUS_BASE_URL": ts.URL, "PROMETHEUS_HEALTH_QUERIES": raw}), slog.New(slog.DiscardHandler))
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := build(kit.NewEnvFrom(map[string]string{"PROMETHEUS_BASE_URL": ts.URL, "PROMETHEUS_HEALTH_QUERIES": `{"a":"up"}`}), slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("good config: %v", err)
	}
}
