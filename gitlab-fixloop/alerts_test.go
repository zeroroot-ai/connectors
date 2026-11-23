package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The existing table mock stands in for GitLab. mock-forge has no GitLab side.
// The mock follows https://docs.gitlab.com/api/dependencies/ : an array of
// {"name","version","package_manager","dependency_file_path","vulnerabilities"
// :[{"name","severity","id","url"}]}, 20 per page by default, and the header
// X-Next-Page.
const (
	projectPath = "/api/v4/projects/acme%2Fshop"
	depsPath    = projectPath + "/dependencies"
)

const projectInfo = `{"id":5,"path_with_namespace":"acme/shop","default_branch":"main"}`

func dep(name, version string, vulns ...string) string {
	return `{"name":"` + name + `","version":"` + version + `","package_manager":"npm","dependency_file_path":"package-lock.json",` +
		`"licenses":[],"vulnerabilities":[` + strings.Join(vulns, ",") + `]}`
}

func vuln(id int, name, sev string) string {
	return `{"name":"` + name + `","severity":"` + sev + `","id":` + strconv.Itoa(id) + `,"url":"https://gitlab.example.com/acme/shop/-/security/vulnerabilities/` + strconv.Itoa(id) + `"}`
}

func alertList(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	var l []map[string]any
	for _, a := range out["alerts"].([]any) {
		l = append(l, a.(map[string]any))
	}
	return l
}

func TestAlertsOpenThenGoneAfterFix(t *testing.T) {
	fixedNow := false
	m, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + depsPath: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			if fixedNow {
				_, _ = w.Write([]byte("[" + dep("lodash", "4.17.21") + "," + dep("left-pad", "1.3.0") + "]"))
				return
			}
			_, _ = w.Write([]byte("[" + dep("lodash", "4.17.20", vuln(77, "Prototype Pollution in lodash (CVE-2026-1111, GHSA-c2f3-g4h5-j6m7)", "high")) + "," + dep("left-pad", "1.3.0") + "]"))
		},
	})
	isErr, msg, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if isErr {
		t.Fatal(msg)
	}
	l := alertList(t, out)
	if len(l) != 1 || out["truncated"] != false || out["ref"] != "main" {
		t.Fatalf("out = %v", out)
	}
	a := l[0]
	if a["id"] != "gitlab:77" || a["state"] != "open" || a["package"] != "lodash" || a["ecosystem"] != "npm" || a["version"] != "4.17.20" ||
		a["version_source"] != "host" || a["purl"] != "pkg:npm/lodash@4.17.20" || a["severity"] != "high" || a["manifest_path"] != "package-lock.json" ||
		a["advisory_ids"].([]any)[0] != "GHSA-c2f3-g4h5-j6m7" || a["advisory_ids"].([]any)[1] != "CVE-2026-1111" {
		t.Errorf("alert = %v", a)
	}
	if m.token != "glpat-secret" {
		t.Errorf("token %q", m.token)
	}
	fixedNow = true
	_, _, out = call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "all"})
	if n := len(alertList(t, out)); n != 0 {
		t.Errorf("alerts after the fix: %v", out)
	}
}

func TestAlertsPagesAndCut(t *testing.T) {
	_, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + depsPath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			p, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if r.URL.Query().Get("per_page") != "100" {
				http.Error(w, `{"message":"bad per_page"}`, http.StatusBadRequest)
				return
			}
			if p < 3 {
				w.Header().Set("X-Next-Page", strconv.Itoa(p+1))
			}
			_, _ = w.Write([]byte("[" + dep("p"+strconv.Itoa(p), "1.0.0", vuln(p, "x", "low")) + "]"))
		},
	})
	_, _, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if n := len(alertList(t, out)); n != 3 || out["truncated"] != false {
		t.Errorf("three pages: %v", out)
	}

	_, call = setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + depsPath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			p, _ := strconv.Atoi(r.URL.Query().Get("page"))
			w.Header().Set("X-Next-Page", strconv.Itoa(p+1))
			_, _ = w.Write([]byte("[" + dep("p"+strconv.Itoa(p), "1.0.0", vuln(p, "x", "low")) + "]"))
		},
	})
	_, _, out = call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if n := len(alertList(t, out)); n != 10 || out["truncated"] != true {
		t.Errorf("cut list: n=%d truncated=%v", n, out["truncated"])
	}
}

func TestAlertsRefusals(t *testing.T) {
	m, call := setup(t, routeMap{"GET " + projectPath: reply(200, projectInfo), "GET " + depsPath: reply(200, "[]")})
	for name, args := range map[string]map[string]any{
		"fixed state":    {"project": "acme/shop", "state": "fixed"},
		"dismissed":      {"project": "acme/shop", "state": "dismissed"},
		"bad state":      {"project": "acme/shop", "state": "x"},
		"not the branch": {"project": "acme/shop", "ref": "dev"},
		"bad project":    {"project": "../x"},
	} {
		if isErr, _, _ := call("list_dependency_alerts", args); !isErr {
			t.Errorf("%s accepted", name)
		}
	}
	if isErr, _, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "ref": "main"}); isErr || out["ref"] != "main" {
		t.Error("the default branch was refused")
	}
	if m.called("GET "+depsPath) != 1 {
		t.Errorf("calls = %v", m.calls)
	}
}
