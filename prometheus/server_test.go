package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func setup(t *testing.T, extra map[string]string) (*mock, func(string, map[string]any) (bool, string)) {
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
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
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
