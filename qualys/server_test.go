package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the "Host List Detection" chapter of the Qualys VM/PC API
// User Guide (https://cdn2.qualys.com/docs/qualys-api-vmpc-user-guide.pdf):
// GET /api/2.0/fo/asset/host/vm/detection/?action=list with basic
// authentication and the header X-Requested-With. The sample XML of the guide
// has the same elements. A request without the header gets an error in the
// SIMPLE_RETURN format.
const sampleXML = `<?xml version="1.0" encoding="UTF-8" ?>
<!DOCTYPE HOST_LIST_VM_DETECTION_OUTPUT SYSTEM "https://qualysapi.qualys.com/api/2.0/fo/asset/host/vm/detection/dtd/output.dtd">
<HOST_LIST_VM_DETECTION_OUTPUT>
  <RESPONSE>
    <DATETIME>2026-11-20T13:34:18Z</DATETIME>
    <HOST_LIST>
      <HOST>
        <ID>524323</ID>
        <IP>10.10.10.9</IP>
        <TRACKING_METHOD>IP</TRACKING_METHOD>
        <OS><![CDATA[Windows 2003 Service Pack 2]]></OS>
        <DNS><![CDATA[win2003.sample.qualys.com]]></DNS>
        <LAST_VM_SCANNED_DATE>2026-11-08T06:37:29Z</LAST_VM_SCANNED_DATE>
        <DETECTION_LIST>
          <DETECTION>
            <UNIQUE_VULN_ID>3978355</UNIQUE_VULN_ID>
            <QID>82054</QID>
            <TYPE>Confirmed</TYPE>
            <SEVERITY>2</SEVERITY>
            <SSL>0</SSL>
            <RESULTS><![CDATA[Tested on port 2222.]]></RESULTS>
            <STATUS>New</STATUS>
            <FIRST_FOUND_DATETIME>2026-11-08T06:37:29Z</FIRST_FOUND_DATETIME>
            <LAST_FOUND_DATETIME>2026-11-08T06:37:29Z</LAST_FOUND_DATETIME>
            <TIMES_FOUND>1</TIMES_FOUND>
          </DETECTION>
          <DETECTION>
            <UNIQUE_VULN_ID>3978399</UNIQUE_VULN_ID>
            <QID>370613</QID>
            <TYPE>Confirmed</TYPE>
            <SEVERITY>5</SEVERITY>
            <STATUS>Active</STATUS>
            <FIRST_FOUND_DATETIME>2026-10-01T06:37:29Z</FIRST_FOUND_DATETIME>
            <LAST_FOUND_DATETIME>2026-11-08T06:37:29Z</LAST_FOUND_DATETIME>
            <TIMES_FOUND>7</TIMES_FOUND>
          </DETECTION>
        </DETECTION_LIST>
      </HOST>
    </HOST_LIST>
  </RESPONSE>
</HOST_LIST_VM_DETECTION_OUTPUT>`

type mock struct {
	method string
	body   string
	query  url.Values
	user   string
	pass   string
	xrw    string
	path   string
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.query, m.path, m.xrw = r.URL.Query(), r.URL.Path, r.Header.Get("X-Requested-With")
	b, _ := io.ReadAll(r.Body)
	m.method, m.body = r.Method, string(b)
	m.user, m.pass, _ = r.BasicAuth()
	w.Header().Set("Content-Type", "text/xml")
	if m.xrw == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><SIMPLE_RETURN><RESPONSE><DATETIME>2026-11-20T13:34:18Z</DATETIME><CODE>2000</CODE><TEXT>X-Requested-With header is missing</TEXT></RESPONSE></SIMPLE_RETURN>`))
		return
	}
	if m.pass != "pw" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><SIMPLE_RETURN><RESPONSE><CODE>1960</CODE><TEXT>Bad Login/Password</TEXT></RESPONSE></SIMPLE_RETURN>`))
		return
	}
	_, _ = w.Write([]byte(sampleXML))
}

func setup(t *testing.T, pass string) (srv *mock, call func(string, map[string]any) (bool, string)) {
	t.Helper()
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"QUALYS_API_URL": ts.URL, "QUALYS_USERNAME": "reader1", "QUALYS_PASSWORD": pass,
	}), slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res)
	}
}

func TestGetHostDetectionsByHostID(t *testing.T) {
	m, call := setup(t, "pw")
	isErr, text := call("get_host_detections", map[string]any{"host_id": "524323", "status": []string{"new", "Active"}, "min_severity": 2})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	want := url.Values{
		"action": {"list"}, "output_format": {"XML"}, "truncation_limit": {"10"}, "ids": {"524323"},
		"status": {"New,Active"}, "severities": {"2-5"}, "show_results": {"0"},
	}
	if m.method != http.MethodGet || m.path != "/api/2.0/fo/asset/host/vm/detection/" || m.query.Encode() != want.Encode() || m.body != "" {
		t.Errorf("request = %s %s?%s body %q, want GET ?%s", m.method, m.path, m.query.Encode(), m.body, want.Encode())
	}
	if m.user != "reader1" || m.xrw == "" {
		t.Errorf("user=%q xrw=%q", m.user, m.xrw)
	}
	for _, want := range []string{`"host_id":"524323"`, `"dns":"win2003.sample.qualys.com"`, `"qid":"82054"`, `"qid":"370613"`,
		`"severity":5`, `"status":"Active"`, `"times_found":7`, `"total_detections":2`, `"truncated":false`} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "Tested on port") {
		t.Errorf("results shown although include_results is false: %s", text)
	}
}

func TestGetHostDetectionsByIPWithResultsAndLimit(t *testing.T) {
	m, call := setup(t, "pw")
	isErr, text := call("get_host_detections", map[string]any{"ip": "10.10.10.9", "include_results": true, "limit": 1})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	want := url.Values{"action": {"list"}, "output_format": {"XML"}, "truncation_limit": {"10"}, "ips": {"10.10.10.9"}}
	if m.method != http.MethodGet || m.query.Encode() != want.Encode() {
		t.Errorf("request = %s ?%s, want GET ?%s", m.method, m.query.Encode(), want.Encode())
	}
	if !strings.Contains(text, "Tested on port 2222.") || !strings.Contains(text, `"truncated":true`) ||
		!strings.Contains(text, `"total_detections":2`) || strings.Contains(text, "370613") {
		t.Errorf("output = %s", text)
	}
}

func TestVendorErrorShown(t *testing.T) {
	_, call := setup(t, "wrong")
	isErr, text := call("get_host_detections", map[string]any{"host_id": "1"})
	if !isErr || !strings.Contains(text, "Bad Login/Password") || !strings.Contains(text, "1960") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestInputValidation(t *testing.T) {
	m, call := setup(t, "pw")
	cases := map[string]map[string]any{
		"none":            {},
		"both":            {"host_id": "1", "ip": "10.0.0.1"},
		"host id letters": {"host_id": "12a"},
		"host id range":   {"host_id": "1-999999"},
		"host id list":    {"host_id": "1,2"},
		"ip cidr":         {"ip": "10.0.0.0/8"},
		"ip range":        {"ip": "10.0.0.1-10.0.0.9"},
		"ip name":         {"ip": "host.example.com"},
		"ip inject":       {"ip": "10.0.0.1&action=purge"},
		"bad status":      {"ip": "10.0.0.1", "status": []string{"Everything"}},
		"severity high":   {"ip": "10.0.0.1", "min_severity": 6},
		"limit high":      {"ip": "10.0.0.1", "limit": 501},
	}
	for name, args := range cases {
		m.path = ""
		if isErr, _ := call("get_host_detections", args); !isErr {
			t.Errorf("%s accepted", name)
		}
		if m.path != "" {
			t.Errorf("%s reached the vendor", name)
		}
	}
}

func TestReadOnly(t *testing.T) {
	m := &mock{}
	ts := httptest.NewTLSServer(m)
	defer ts.Close()
	register, err := buildWith(kit.NewEnvFrom(map[string]string{"QUALYS_API_URL": ts.URL, "QUALYS_USERNAME": "u", "QUALYS_PASSWORD": "p"}),
		slog.New(slog.DiscardHandler), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	tools := kittest.Tools(t, kittest.Connect(t, register))
	if len(tools) != 1 || !tools["get_host_detections"].Annotations.ReadOnlyHint {
		t.Errorf("tools = %v", tools)
	}
}

func TestConfigRefusesPlainHTTP(t *testing.T) {
	_, err := build(kit.NewEnvFrom(map[string]string{"QUALYS_API_URL": "http://qualysapi.qualys.com", "QUALYS_USERNAME": "u", "QUALYS_PASSWORD": "p"}),
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Error("plain http accepted")
	}
}
