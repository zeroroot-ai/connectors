package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

const endpointID = "6bf2a1c8d9e04f3b8a7c5d2e1f0a9b8c"

// The mock follows the public API of Cortex XDR
// (https://cortex-docs.paloaltonetworks.com/xdr-3-api/cortex-xdr-3.x-apis/incident-management and
// .../endpoint-management): each call is a POST with {"request_data": {...}}
// and answers {"reply": {...}}. A wrong key gets HTTP 401.
type mock struct {
	calls   []string
	data    []map[string]any
	headers []http.Header
}

const incidentJS = `{"incident_id":"42","incident_name":"Incident 42","description":"Credential theft on 3 hosts","status":"new","severity":"high",` +
	`"creation_time":1795168800000,"modification_time":1795172400000,"host_count":3,"alert_count":7,"assigned_user_mail":"a@example.com","hosts":["h1"]}`

const alertJS = `{"alert_id":"9001","name":"Mimikatz","category":"Credential Access","severity":"high","source":"XDR Agent","action":"BLOCKED",` +
	`"detection_timestamp":1795168900000,"endpoint_id":"` + endpointID + `","host_name":"win-1","user_name":"admin","description":"d","host_ip":["10.0.0.5"],"events":[]}`

const endpointJS = `{"endpoint_id":"` + endpointID + `","endpoint_name":"win-1","endpoint_type":"AGENT_TYPE_WORKSTATION","endpoint_status":"CONNECTED",` +
	`"os_type":"AGENT_OS_WINDOWS","ip":["10.0.0.5"],"last_seen":1795168999000,"is_isolated":"AGENT_UNISOLATED"}`

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.headers = append(m.headers, r.Header.Clone())
	var body struct {
		RequestData map[string]any `json:"request_data"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	m.data = append(m.data, body.RequestData)
	if r.Header.Get("x-xdr-auth-id") != "7" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"reply":{"err_code":401,"err_msg":"Public API request unauthorized"}}`))
		return
	}
	switch r.URL.Path {
	case "/public_api/v1/incidents/get_incidents":
		_, _ = w.Write([]byte(`{"reply":{"total_count":60,"result_count":1,"incidents":[` + incidentJS + `]}}`))
	case "/public_api/v1/incidents/get_incident_extra_data":
		_, _ = w.Write([]byte(`{"reply":{"incident":` + incidentJS + `,"alerts":{"total_count":7,"data":[` + alertJS + `]},"network_artifacts":{},"file_artifacts":{}}}`))
	case "/public_api/v1/alerts/get_alerts":
		_, _ = w.Write([]byte(`{"reply":{"total_count":1,"result_count":1,"alerts":[` + alertJS + `]}}`))
	case "/public_api/v1/endpoints/get_endpoint":
		_, _ = w.Write([]byte(`{"reply":{"total_count":1,"result_count":1,"endpoints":[` + endpointJS + `]}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setupWith(t *testing.T, keyType string) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"CORTEX_API_URL": ts.URL, "CORTEX_API_KEY_ID": "7", "CORTEX_API_KEY": "the-secret-key", "CORTEX_API_KEY_TYPE": keyType,
	}), slog.New(slog.DiscardHandler), ts.Client())
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

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	return setupWith(t, "standard")
}

func filters(t *testing.T, d map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range d["filters"].([]any) {
		out = append(out, f.(map[string]any))
	}
	return out
}

func TestListIncidents(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("list_incidents", map[string]any{
		"status": "new", "created_after": "2026-11-20T10:00:00Z", "created_before": "2026-11-21T10:00:00Z", "description_contains": "Credential", "limit": 10, "offset": 20,
	})
	if isErr {
		t.Fatal(text)
	}
	var out incidentsOut
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Total != 60 || !out.HasMore || out.Incidents[0].ID != "42" || out.Incidents[0].Severity != "high" || out.Incidents[0].HostCount != 3 {
		t.Errorf("out = %+v", out)
	}
	d := m.data[0]
	if d["search_from"] != float64(20) || d["search_to"] != float64(30) {
		t.Errorf("window = %v %v", d["search_from"], d["search_to"])
	}
	f := filters(t, d)
	if len(f) != 4 || f[0]["field"] != "status" || f[0]["operator"] != "eq" || f[0]["value"] != "new" ||
		f[1]["field"] != "creation_time" || f[1]["operator"] != "gte" || f[1]["value"] != float64(1795168800000) ||
		f[2]["operator"] != "lte" || f[3]["field"] != "description" || f[3]["operator"] != "contains" || f[3]["value"] != "Credential" {
		t.Errorf("filters = %v", f)
	}
	if s := d["sort"].(map[string]any); s["field"] != "creation_time" || s["keyword"] != "desc" {
		t.Errorf("sort = %v", s)
	}
	if m.calls[0] != "POST /public_api/v1/incidents/get_incidents" {
		t.Errorf("calls = %v", m.calls)
	}
}

func TestGetIncident(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("get_incident", map[string]any{"incident_id": "42", "alerts_limit": 5})
	if isErr {
		t.Fatal(text)
	}
	var out incidentOut
	kittest.Structured(t, res, &out)
	if out.Incident.ID != "42" || out.AlertsTotal != 7 || len(out.Alerts) != 1 || out.Alerts[0].Name != "Mimikatz" || out.Alerts[0].HostName != "win-1" {
		t.Errorf("out = %+v", out)
	}
	if d := m.data[0]; d["incident_id"] != "42" || d["alerts_limit"] != float64(5) {
		t.Errorf("data = %v", d)
	}
}

func TestListAlerts(t *testing.T) {
	m, call, _ := setup(t)
	_, text, res := call("list_alerts", map[string]any{"severity": "high", "created_after": "2026-11-20T10:00:00Z"})
	var out alertsOut
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.HasMore || out.Alerts[0].AlertID != "9001" || out.Alerts[0].Category != "Credential Access" || out.Alerts[0].DetectionTimestamp != 1795168900000 {
		t.Errorf("out = %+v %s", out, text)
	}
	f := filters(t, m.data[0])
	if len(f) != 2 || f[0]["field"] != "severity" || f[0]["operator"] != "in" || f[0]["value"].([]any)[0] != "high" || f[1]["operator"] != "gte" {
		t.Errorf("filters = %v", f)
	}
}

func TestListEndpoints(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("list_endpoints", map[string]any{"hostname": "win-1", "status": "connected", "isolation": "unisolated"})
	if isErr {
		t.Fatal(text)
	}
	var out endpointsOut
	kittest.Structured(t, res, &out)
	if out.Count != 1 || out.Endpoints[0].EndpointID != endpointID || out.Endpoints[0].IsolatedStatus != "AGENT_UNISOLATED" || out.Endpoints[0].LastSeen != 1795168999000 {
		t.Errorf("out = %+v", out)
	}
	f := filters(t, m.data[0])
	if len(f) != 3 || f[0]["field"] != "hostname" || f[1]["field"] != "endpoint_status" || f[2]["field"] != "isolate" || f[2]["value"].([]any)[0] != "unisolated" {
		t.Errorf("filters = %v", f)
	}
	call("list_endpoints", map[string]any{"endpoint_id": endpointID})
	if f := filters(t, m.data[1]); len(f) != 1 || f[0]["field"] != "endpoint_id_list" {
		t.Errorf("filters = %v", f)
	}
	// With no filter, the list is "filters": [] and not null.
	call("list_endpoints", map[string]any{})
	if _, ok := m.data[2]["filters"].([]any); !ok {
		t.Errorf("filters = %v", m.data[2]["filters"])
	}
}

func TestStandardAuthHeaders(t *testing.T) {
	m, call, _ := setup(t)
	call("list_alerts", map[string]any{})
	h := m.headers[0]
	if h.Get("x-xdr-auth-id") != "7" || h.Get("Authorization") != "the-secret-key" || h.Get("x-xdr-nonce") != "" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", h)
	}
}

// TestAdvancedAuthNeverSendsTheKey checks the headers of an advanced key: the
// authorization header is the SHA-256 of key, nonce and timestamp.
func TestAdvancedAuthNeverSendsTheKey(t *testing.T) {
	m, call, _ := setupWith(t, "advanced")
	call("list_alerts", map[string]any{})
	h := m.headers[0]
	nonce, ts := h.Get("x-xdr-nonce"), h.Get("x-xdr-timestamp")
	sum := sha256.Sum256([]byte("the-secret-key" + nonce + ts))
	if h.Get("Authorization") != hex.EncodeToString(sum[:]) || strings.Contains(h.Get("Authorization"), "the-secret-key") {
		t.Errorf("authorization = %q", h.Get("Authorization"))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{64}$`).MatchString(nonce) {
		t.Errorf("nonce = %q", nonce)
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.UnixMilli(ms)) > time.Minute || time.Since(time.UnixMilli(ms)) < -time.Minute {
		t.Errorf("timestamp = %q", ts)
	}
	call("list_alerts", map[string]any{})
	if m.headers[1].Get("x-xdr-nonce") == nonce {
		t.Error("the nonce repeats")
	}
}

func TestReadsRefuseBadInput(t *testing.T) {
	m, call, _ := setup(t)
	bad := map[string][]map[string]any{
		"list_incidents": {{"status": "closed"}, {"status": "new,under_investigation"}, {"created_after": "yesterday"}, {"created_before": "2026-11-20"},
			{"limit": 101}, {"limit": -1}, {"offset": -1}, {"offset": 10001}, {"description_contains": "a\nb"}},
		"get_incident":   {{}, {"incident_id": "abc"}, {"incident_id": "4 2"}, {"incident_id": "../42"}, {"incident_id": "42", "alerts_limit": 101}},
		"list_alerts":    {{"severity": "urgent"}, {"severity": "high,low"}, {"created_after": "x"}},
		"list_endpoints": {{"endpoint_id": "a b"}, {"endpoint_id": "x"}, {"hostname": "a b"}, {"hostname": "a/../b"}, {"status": "CONNECTED"}, {"isolation": "yes"}},
	}
	for tool, ins := range bad {
		for _, in := range ins {
			if isErr, _, _ := call(tool, in); !isErr {
				t.Errorf("%s accepted %v", tool, in)
			}
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached Cortex: %v", m.calls)
	}
}

func TestAuthFailureShowsNoKey(t *testing.T) {
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	defer ts.Close()
	register, err := buildWith(kit.NewEnvFrom(map[string]string{"CORTEX_API_URL": ts.URL, "CORTEX_API_KEY_ID": "8", "CORTEX_API_KEY": "the-secret-key"}),
		slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	res := kittest.Call(t, kittest.Connect(t, register), "list_alerts", map[string]any{})
	text := kittest.Text(t, res)
	if !res.IsError || !strings.Contains(text, "HTTP 401") || strings.Contains(text, "the-secret-key") {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
}

func TestReadOnly(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 4 {
		t.Fatalf("tools = %d", len(tools))
	}
	for n, tl := range tools {
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not read only", n)
		}
	}
	call("list_incidents", map[string]any{})
	call("get_incident", map[string]any{"incident_id": "42"})
	call("list_alerts", map[string]any{})
	call("list_endpoints", map[string]any{})
	for _, c := range m.calls {
		// Each call is a search with a POST. No path of a change is used.
		if strings.Contains(c, "isolate") || strings.Contains(c, "update") || strings.Contains(c, "insert") {
			t.Errorf("a read tool called %s", c)
		}
	}
}

func TestConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := build(kit.NewEnvFrom(nil), log); err == nil {
		t.Error("empty environment accepted")
	}
	base := map[string]string{"CORTEX_API_URL": "https://api-acme.xdr.us.paloaltonetworks.com", "CORTEX_API_KEY_ID": "7", "CORTEX_API_KEY": "k"}
	if _, err := build(kit.NewEnvFrom(base), log); err != nil {
		t.Errorf("good config: %v", err)
	}
	for name, extra := range map[string]map[string]string{
		"plain http":   {"CORTEX_API_URL": "http://api-acme.xdr.us.paloaltonetworks.com"},
		"bad key id":   {"CORTEX_API_KEY_ID": "7; drop"},
		"bad key type": {"CORTEX_API_KEY_TYPE": "basic"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		if _, err := build(kit.NewEnvFrom(env), log); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
