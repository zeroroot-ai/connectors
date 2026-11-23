package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The existing table mock stands in for GitLab. mock-forge has no GitLab side.
// The dependency list follows https://docs.gitlab.com/api/dependencies/ : an
// array of {"name","version","package_manager","dependency_file_path",
// "vulnerabilities"}, and the header X-Next-Page. The vulnerabilities follow the
// GraphQL reference https://docs.gitlab.com/api/graphql/reference/ (Project.
// vulnerabilities, Vulnerability, VulnerabilityIdentifier,
// VulnerabilityLocationDependencyScanning, VulnerableDependency).
const (
	projectPath = "/api/v4/projects/acme%2Fshop"
	depsPath    = projectPath + "/dependencies"
	graphPath   = "/api/graphql"
)

const projectInfo = `{"id":5,"path_with_namespace":"acme/shop","default_branch":"main"}`

func dep(name, version string, vulns ...string) string {
	return `{"name":"` + name + `","version":"` + version + `","package_manager":"npm","dependency_file_path":"package-lock.json",` +
		`"licenses":[],"vulnerabilities":[` + strings.Join(vulns, ",") + `]}`
}

func depList(items ...string) routeHandler { return reply(200, "["+strings.Join(items, ",")+"]") }

type routeHandler = func(http.ResponseWriter, *http.Request, map[string]any)

func vulnNode(id int, state string, resolved bool, pkg, version, file, identifiers string) string {
	return `{"id":"gid://gitlab/Vulnerability/` + strconv.Itoa(id) + `","state":"` + state + `","resolvedOnDefaultBranch":` + strconv.FormatBool(resolved) + `,"severity":"HIGH","title":"Flaw in ` + pkg + `",` +
		`"webUrl":"https://gitlab.example.com/acme/shop/-/security/vulnerabilities/` + strconv.Itoa(id) + `",` +
		`"identifiers":[` + identifiers + `],"location":{"__typename":"VulnerabilityLocationDependencyScanning","file":"` + file + `",` +
		`"dependency":{"package":{"name":"` + pkg + `"},"version":"` + version + `"}}}`
}

const (
	cveGhsa = `{"externalType":"cve","externalId":"CVE-2026-1111","name":"CVE-2026-1111","url":""},` +
		`{"externalType":"ghsa","externalId":"GHSA-c2f3-g4h5-j6m7","name":"GHSA-c2f3-g4h5-j6m7","url":""}`
)

func graph(nodes, next string) string {
	pi := `{"hasNextPage":false,"endCursor":null}`
	if next != "" {
		pi = `{"hasNextPage":true,"endCursor":"` + next + `"}`
	}
	return `{"data":{"project":{"vulnerabilities":{"pageInfo":` + pi + `,"nodes":[` + nodes + `]}}}}`
}

func alertList(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw := out["alerts"].([]any)
	l := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		l = append(l, a.(map[string]any))
	}
	return l
}

// The answers follow the GraphQL reference: GitLab sets the state RESOLVED when a
// person or a policy resolves a vulnerability. A fix that a scan sees leaves the
// state DETECTED and sets resolvedOnDefaultBranch to true.
func stateFixture() string {
	return strings.Join([]string{
		vulnNode(77, "DETECTED", false, "lodash", "4.17.20", "package-lock.json", cveGhsa), // open
		vulnNode(78, "DETECTED", true, "github.com/Acme/Lib", "v1.2.0", "go.mod", cveGhsa), // fixed by a scan
		vulnNode(79, "DISMISSED", false, "left-pad", "1.3.0", "package-lock.json", ""),     // dismissed
		vulnNode(80, "RESOLVED", false, "minimist", "1.0.0", "package-lock.json", cveGhsa), // resolved by a person: open
		vulnNode(81, "DISMISSED", true, "chalk", "1.0.0", "package-lock.json", cveGhsa),    // dismissed wins
		vulnNode(82, "CONFIRMED", false, "request", "2.0.0", "package-lock.json", cveGhsa), // open
	}, ",")
}

func TestAlertsStatesAndFields(t *testing.T) {
	m, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + depsPath:    depList(dep("lodash", "4.17.20", `{"id":77}`), dep("left-pad", "1.3.0")),
		"POST " + graphPath: func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			if !strings.Contains(fmt.Sprint(body["query"]), "resolvedOnDefaultBranch") {
				http.Error(w, `{"message":"the query must ask resolvedOnDefaultBranch"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(graph(stateFixture(), "")))
		},
	})
	_, _, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	byID := map[string]map[string]any{}
	for _, x := range alertList(t, out) {
		byID[x["id"].(string)] = x
	}
	if len(byID) != 3 || byID["gitlab:77"] == nil || byID["gitlab:80"] == nil || byID["gitlab:82"] == nil || out["truncated"] != false || out["ref"] != "main" {
		t.Fatalf("open = %v", out)
	}
	checkOpenAlert(t, byID["gitlab:77"])
	if byID["gitlab:80"]["state"] != "open" || m.token != "glpat-secret" {
		t.Errorf("RESOLVED with no fix on the default branch must be open: %v, token %q", byID["gitlab:80"], m.token)
	}
	checkOtherStates(t, call)
}

func checkOpenAlert(t *testing.T, a map[string]any) {
	t.Helper()
	ids := fmt.Sprint(a["advisory_ids"])
	if a["state"] != "open" || a["package"] != "lodash" || a["ecosystem"] != "npm" || a["version"] != "4.17.20" ||
		a["version_source"] != "host" || a["purl"] != "pkg:npm/lodash@4.17.20" || a["severity"] != "high" || a["manifest_path"] != "package-lock.json" ||
		ids != "[GHSA-c2f3-g4h5-j6m7 CVE-2026-1111]" || a["verifiable"] != true {
		t.Errorf("alert = %v", a)
	}
}

func checkOtherStates(t *testing.T, call func(string, map[string]any) (bool, string, map[string]any)) {
	t.Helper()
	_, _, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "fixed"})
	l := alertList(t, out)
	if len(l) != 1 || l[0]["id"] != "gitlab:78" || l[0]["state"] != "fixed" {
		t.Fatalf("fixed = %v", out)
	}
	// A resolved vulnerability is not in the dependency list. The file name gives the ecosystem.
	if l[0]["ecosystem"] != "go" || l[0]["purl"] != "pkg:golang/github.com/acme/lib@v1.2.0" {
		t.Errorf("fixed alert = %v", l[0])
	}
	_, _, out = call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "dismissed"})
	got := alertList(t, out)
	ids := make([]string, 0, len(got))
	for _, x := range got {
		ids = append(ids, x["id"].(string))
		if x["id"] == "gitlab:79" && (x["verifiable"] != false || len(x["advisory_ids"].([]any)) != 0) {
			t.Errorf("an alert with no advisory id must have verifiable false: %v", x)
		}
	}
	if fmt.Sprint(ids) != "[gitlab:79 gitlab:81]" {
		t.Errorf("dismissed = %v", ids)
	}
	_, _, out = call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "all"})
	if n := len(alertList(t, out)); n != 6 {
		t.Errorf("all = %d alerts", n)
	}
}

func TestAlertsErrorsAreNeverACleanList(t *testing.T) {
	graphOK := reply(200, graph("", ""))
	for name, c := range map[string]struct {
		routes routeMap
		want   string
	}{
		"no vulnerabilities key (token cannot read the dashboard)": {routeMap{
			"GET " + depsPath:   reply(200, `[{"name":"lodash","version":"1","package_manager":"npm","dependency_file_path":"package-lock.json"}]`),
			"POST " + graphPath: graphOK,
		}, "Security Dashboard"},
		"no dependencies (no scan)": {routeMap{"GET " + depsPath: reply(200, `[]`), "POST " + graphPath: graphOK}, "no dependency scan"},
		"graphql errors": {routeMap{
			"GET " + depsPath:   depList(dep("a", "1")),
			"POST " + graphPath: reply(200, `{"errors":[{"message":"field vulnerabilities is not available"}],"data":null}`),
		}, "GraphQL errors"},
		"project is null": {routeMap{
			"GET " + depsPath:   depList(dep("a", "1")),
			"POST " + graphPath: reply(200, `{"data":{"project":null}}`),
		}, "no vulnerabilities field"},
		"vulnerabilities is null": {routeMap{
			"GET " + depsPath:   depList(dep("a", "1")),
			"POST " + graphPath: reply(200, `{"data":{"project":{"vulnerabilities":null}}}`),
		}, "no vulnerabilities field"},
		"next page with no cursor": {routeMap{
			"GET " + depsPath:   depList(dep("a", "1")),
			"POST " + graphPath: reply(200, `{"data":{"project":{"vulnerabilities":{"pageInfo":{"hasNextPage":true,"endCursor":""},"nodes":[]}}}}`),
		}, "no cursor"},
		"forbidden": {routeMap{"GET " + depsPath: reply(403, `{"message":"403 Forbidden"}`), "POST " + graphPath: graphOK}, "403"},
		"graphql http 401": {routeMap{
			"GET " + depsPath:   depList(dep("a", "1")),
			"POST " + graphPath: reply(401, `{"message":"401 Unauthorized"}`),
		}, "401"},
	} {
		c.routes["GET "+projectPath] = reply(200, projectInfo)
		_, call := setup(t, c.routes)
		isErr, msg, _ := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
		if !isErr || !strings.Contains(msg, c.want) {
			t.Errorf("%s: isErr=%v msg=%q, want %q", name, isErr, msg, c.want)
		}
	}
}

func TestAlertsPagesAndCut(t *testing.T) {
	_, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + depsPath:    depList(dep("a", "1")),
		"POST " + graphPath: func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			vars := body["variables"].(map[string]any)
			switch vars["after"] {
			case nil:
				_, _ = w.Write([]byte(graph(vulnNode(1, "DETECTED", false, "a", "1", "package-lock.json", cveGhsa), "C2")))
			case "C2":
				_, _ = w.Write([]byte(graph(vulnNode(2, "DETECTED", false, "a", "1", "package-lock.json", cveGhsa), "")))
			default:
				http.Error(w, `{"message":"bad cursor"}`, http.StatusBadRequest)
			}
		},
	})
	_, _, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if n := len(alertList(t, out)); n != 2 || out["truncated"] != false {
		t.Errorf("two pages: %v", out)
	}

	calls := 0
	_, call = setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + depsPath:    depList(dep("a", "1")),
		"POST " + graphPath: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			calls++
			_, _ = w.Write([]byte(graph(vulnNode(calls, "DETECTED", false, "a", "1", "package-lock.json", cveGhsa), "NEXT")))
		},
	})
	_, _, out = call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if n := len(alertList(t, out)); n != 10 || out["truncated"] != true || calls != 10 {
		t.Errorf("cut list: n=%d truncated=%v calls=%d", n, out["truncated"], calls)
	}
}

func TestAlertsRefusals(t *testing.T) {
	m, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo), "GET " + depsPath: depList(dep("a", "1")),
		"POST " + graphPath: reply(200, graph("", "")),
	})
	for name, args := range map[string]map[string]any{
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
	if m.called("POST "+graphPath) != 1 {
		t.Errorf("calls = %v", m.calls)
	}
}
