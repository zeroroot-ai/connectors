package main

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the InsightVM Console API v3
// (https://help.rapid7.com/insightvm/en-us/api/api-v3.json): a page answer is
// {"resources":[...],"page":{"number","size","totalPages","totalResources"}},
// an asset has hostName, ip, os, type, riskScore and vulnerabilities
// {critical,severe,moderate,total,exploits,malwareKits}, a finding has id,
// status, since and instances, and a vulnerability has cvss.v2 and cvss.v3.
type mock struct {
	calls   []string
	queries []url.Values
	bodies  []map[string]any
	auth    string
}

const assetJS = `{"id":42,"ip":"10.0.0.5","hostName":"db-1","os":"Ubuntu Linux 22.04","type":"guest","riskScore":12345.5,` +
	`"vulnerabilities":{"critical":2,"severe":5,"moderate":9,"total":16,"exploits":1,"malwareKits":0},"history":[],"services":[]}`

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.queries = append(m.queries, r.URL.Query())
	m.auth = r.Header.Get("Authorization")
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	m.bodies = append(m.bodies, body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/3/assets/search":
		_, _ = w.Write([]byte(`{"resources":[` + assetJS + `],"page":{"number":0,"size":25,"totalPages":1,"totalResources":1}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3/assets/42":
		_, _ = w.Write([]byte(assetJS))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3/assets/5000000001":
		_, _ = w.Write([]byte(strings.Replace(assetJS, `"id":42`, `"id":5000000001`, 1)))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3/assets/5000000001/vulnerabilities":
		_, _ = w.Write([]byte(`{"resources":[],"page":{"number":0,"size":25,"totalPages":0,"totalResources":0}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3/assets/42/vulnerabilities":
		_, _ = w.Write([]byte(`{"resources":[{"id":"ssh-cve-2020-14145","status":"vulnerable-exploited","since":"2026-11-01T00:00:00.000Z","instances":2,"results":[],"links":[]}],` +
			`"page":{"number":0,"size":25,"totalPages":3,"totalResources":60}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3/vulnerabilities/ssh-cve-2020-14145":
		_, _ = w.Write([]byte(`{"id":"ssh-cve-2020-14145","title":"OpenSSH information disclosure","severity":"Moderate","severityScore":5,"riskScore":322.5,` +
			`"cves":["CVE-2020-14145"],"published":"2020-06-29","modified":"2024-01-01","fixAvailable":true,"exploits":0,"malwareKits":0,` +
			`"description":{"html":"<p>x</p>","text":"A flaw in OpenSSH."},"cvss":{"v2":{"score":4.3,"vector":"AV:N/AC:M/Au:N/C:P/I:N/A:N"},"v3":{"score":5.9,"vector":"CVSS:3.0/AV:N"}}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"message":"Resource not found"}`))
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (isErr bool, text string, res *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"INSIGHTVM_URL": ts.URL, "INSIGHTVM_USER": "svc", "INSIGHTVM_PASSWORD": "s3cret",
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

func TestFindAssets(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("find_assets", map[string]any{"ip_address": "10.0.0.5", "host_name": "db", "size": 10})
	if isErr {
		t.Fatal(text)
	}
	var out findAssetsOut
	kittest.Structured(t, res, &out)
	if len(out.Assets) != 1 || out.Assets[0].ID != 42 || out.Assets[0].HostName != "db-1" || out.Assets[0].Vulnerabilities.Critical != 2 ||
		out.Assets[0].Vulnerabilities.Total != 16 || out.TotalPages != 1 || out.TotalRecords != 1 {
		t.Errorf("out = %+v", out)
	}
	if m.calls[0] != "POST /api/3/assets/search" || m.queries[0].Get("size") != "10" || m.queries[0].Get("page") != "0" {
		t.Errorf("call %v query %v", m.calls, m.queries[0])
	}
	f := m.bodies[0]["filters"].([]any)
	if m.bodies[0]["match"] != "all" || len(f) != 2 || f[0].(map[string]any)["field"] != "ip-address" || f[0].(map[string]any)["operator"] != "is" ||
		f[1].(map[string]any)["field"] != "host-name" || f[1].(map[string]any)["operator"] != "starts-with" || f[1].(map[string]any)["value"] != "db" {
		t.Errorf("body = %v", m.bodies[0])
	}
	if !strings.HasPrefix(m.auth, "Basic ") {
		t.Errorf("auth = %q", m.auth)
	}
}

func TestGetAssetAndFindings(t *testing.T) {
	m, call, _ := setup(t)
	_, _, res := call("get_asset", map[string]any{"asset_id": 42})
	var a assetOut
	kittest.Structured(t, res, &a)
	if a.ID != 42 || a.IP != "10.0.0.5" || a.RiskScore != 12345.5 || a.Vulnerabilities.Exploits != 1 {
		t.Errorf("asset = %+v", a)
	}
	_, _, res = call("list_asset_vulnerabilities", map[string]any{"asset_id": 42, "page": 1, "size": 5})
	var f findingsOut
	kittest.Structured(t, res, &f)
	if len(f.Findings) != 1 || f.Findings[0].VulnerabilityID != "ssh-cve-2020-14145" || f.Findings[0].Instances != 2 || f.TotalRecords != 60 || f.TotalPages != 3 {
		t.Errorf("findings = %+v", f)
	}
	if q := m.queries[1]; q.Get("page") != "1" || q.Get("size") != "5" {
		t.Errorf("query = %v", q)
	}
	if isErr, text, _ := call("get_asset", map[string]any{"asset_id": 7}); !isErr || !strings.Contains(text, "HTTP 404") {
		t.Errorf("unknown asset: isErr=%v %s", isErr, text)
	}
}

func TestGetVulnerability(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text, res := call("get_vulnerability", map[string]any{"vulnerability_id": "ssh-cve-2020-14145"})
	if isErr {
		t.Fatal(text)
	}
	var v vulnOut
	kittest.Structured(t, res, &v)
	if v.ID != "ssh-cve-2020-14145" || v.Severity != "Moderate" || v.CVSSv3 == nil || v.CVSSv3.Score != 5.9 || v.CVSSv2.Score != 4.3 ||
		len(v.CVEs) != 1 || !v.FixAvailable || v.Description != "A flaw in OpenSSH." {
		t.Errorf("vulnerability = %+v", v)
	}
}

func TestReadsRefuseBadInput(t *testing.T) {
	m, call, _ := setup(t)
	bad := map[string][]map[string]any{
		"find_assets":                {{}, {"ip_address": "10.0.0.5 or 1=1"}, {"ip_address": "x"}, {"host_name": "a b"}, {"host_name": "a/../b"}, {"host_name": "a", "size": 101}, {"host_name": "a", "page": -1}},
		"get_asset":                  {{}, {"asset_id": 0}, {"asset_id": -4}},
		"list_asset_vulnerabilities": {{"asset_id": 0}, {"asset_id": 42, "size": 0 - 1}},
		"get_vulnerability":          {{}, {"vulnerability_id": "../assets"}, {"vulnerability_id": "a b"}, {"vulnerability_id": "a?x=1"}, {"vulnerability_id": strings.Repeat("a", 201)}},
	}
	for tool, ins := range bad {
		for _, in := range ins {
			if isErr, _, _ := call(tool, in); !isErr {
				t.Errorf("%s accepted %v", tool, in)
			}
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached InsightVM: %v", m.calls)
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
	call("find_assets", map[string]any{"host_name": "db"})
	call("get_asset", map[string]any{"asset_id": 42})
	call("list_asset_vulnerabilities", map[string]any{"asset_id": 42})
	call("get_vulnerability", map[string]any{"vulnerability_id": "ssh-cve-2020-14145"})
	for _, c := range m.calls {
		// The search is a POST with a read effect. No other call writes.
		if !strings.HasPrefix(c, "GET ") && c != "POST /api/3/assets/search" {
			t.Errorf("a read tool sent %s", c)
		}
	}
}

func TestCAFile(t *testing.T) {
	ts := httptest.NewTLSServer(&mock{})
	defer ts.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"INSIGHTVM_URL": ts.URL, "INSIGHTVM_USER": "u", "INSIGHTVM_PASSWORD": "p", "INSIGHTVM_CA_FILE": path}
	// With the CA file the server trusts the certificate. No test client is given.
	register, err := buildWith(kit.NewEnvFrom(env), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	if isErr, text := func() (bool, string) {
		res := kittest.Call(t, kittest.Connect(t, register), "get_asset", map[string]any{"asset_id": 42})
		return res.IsError, kittest.Text(t, res)
	}(); isErr {
		t.Errorf("the CA file was not used: %s", text)
	}
	// Without it, the private certificate is refused.
	delete(env, "INSIGHTVM_CA_FILE")
	register, err = buildWith(kit.NewEnvFrom(env), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := kittest.Call(t, kittest.Connect(t, register), "get_asset", map[string]any{"asset_id": 42}); !res.IsError {
		t.Error("an unknown certificate was accepted")
	}
	for name, content := range map[string]string{"not pem": "hello", "missing": ""} {
		p := filepath.Join(t.TempDir(), "x.pem")
		if name == "not pem" {
			_ = os.WriteFile(p, []byte(content), 0o600)
		}
		env["INSIGHTVM_CA_FILE"] = p
		if _, err := buildWith(kit.NewEnvFrom(env), slog.New(slog.DiscardHandler), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := build(kit.NewEnvFrom(nil), log); err == nil {
		t.Error("empty environment accepted")
	}
	plain := map[string]string{"INSIGHTVM_URL": "http://console:3780", "INSIGHTVM_USER": "u", "INSIGHTVM_PASSWORD": "p"}
	if _, err := build(kit.NewEnvFrom(plain), log); err == nil {
		t.Error("plain http accepted")
	}
}

// The spec types the asset id as int64. An id above 2^31 works.
func TestAssetIDIsInt64(t *testing.T) {
	m, call, _ := setup(t)
	const big = 5000000001
	_, text, res := call("get_asset", map[string]any{"asset_id": big})
	var a assetOut
	kittest.Structured(t, res, &a)
	if a.ID != big || m.calls[0] != "GET /api/3/assets/5000000001" {
		t.Errorf("asset = %+v calls %v %s", a, m.calls, text)
	}
	if isErr, text, _ := call("list_asset_vulnerabilities", map[string]any{"asset_id": big}); isErr {
		t.Errorf("findings: %s", text)
	}
	if m.calls[1] != "GET /api/3/assets/5000000001/vulnerabilities" {
		t.Errorf("calls = %v", m.calls)
	}
}
