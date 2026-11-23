package main

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
	"github.com/zeroroot-ai/connectors/internal/mockforge/mocktest"
)

// The fixture ecosystem of the forge mock is go. The mock reads go.mod on the
// default branch.
const (
	vulnGoMod  = "module shop\n\nrequire github.com/acme/lib v1.2.0\n"
	fixedGoMod = "module shop\n\nrequire github.com/acme/lib v1.2.4\n"
	alertsPath = repo + "/dependabot/alerts"
)

func alertForge(t *testing.T) (*mocktest.Server, forgeCall) {
	t.Helper()
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{"go.mod": vulnGoMod}, nil)
	srv.Post("/_mock/advisories", map[string]any{
		"ghsa_id": "GHSA-c2f3-g4h5-j6m7", "cve_id": "CVE-2026-1111", "package": "github.com/acme/lib", "ecosystem": "go",
		"vulnerable_version_range": ">= 1.0.0, < 1.2.4", "first_patched_version": "1.2.4", "severity": "high",
	})
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITHUB_API_URL": srv.URL, "GITHUB_TOKEN": "mock-token", "GITHUB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return srv, func(tool string, args map[string]any) (bool, string, map[string]any) {
		t.Helper()
		res := kittest.Call(t, cs, tool, args)
		var out map[string]any
		if !res.IsError {
			kittest.Structured(t, res, &out)
		}
		return res.IsError, kittest.Text(t, res), out
	}
}

func alertsOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	var list []map[string]any
	for _, a := range out["alerts"].([]any) {
		list = append(list, a.(map[string]any))
	}
	return list
}

func TestAlertsOpenThenFixedAfterMergedFix(t *testing.T) {
	srv, call := alertForge(t)
	isErr, text, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if isErr {
		t.Fatal(text)
	}
	list := alertsOf(t, out)
	if len(list) != 1 || out["truncated"] != false || out["ref"] != "main" || out["state"] != "open" {
		t.Fatalf("open: %v", out)
	}
	a := list[0]
	if a["state"] != "open" || a["package"] != "github.com/acme/lib" || a["ecosystem"] != "go" || a["severity"] != "high" ||
		a["vulnerable_version_range"] != ">= 1.0.0, < 1.2.4" || a["first_patched_version"] != "1.2.4" || a["manifest_path"] != "go.mod" ||
		a["version"] != "v1.2.0" || a["version_source"] != "manifest" || a["purl_type"] != "golang" ||
		a["purl"] != "pkg:golang/github.com/acme/lib@v1.2.0" || a["id"] != "dependabot:1" {
		t.Errorf("alert: %v", a)
	}
	ids := fmt.Sprint(a["advisory_ids"])
	if ids != "[GHSA-c2f3-g4h5-j6m7 CVE-2026-1111]" || !strings.Contains(a["url"].(string), "/security/dependabot/1") {
		t.Errorf("ids %s url %v", ids, a["url"])
	}

	// A pull request that moves go.mod to the patched version closes the alert.
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"branch": "fix", "files": map[string]any{"go.mod": fixedGoMod}})
	_, _, open := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if n := len(alertsOf(t, open)); n != 1 {
		t.Fatalf("a fix branch must not close the alert: %v", open)
	}
	_, pull := srv.Do(http.MethodPost, "/repos/acme/shop/pulls", map[string]any{"title": "fix", "head": "fix", "base": "main"})
	n := strconv.Itoa(int(pull["number"].(float64)))
	if st, body := srv.Do(http.MethodPut, "/repos/acme/shop/pulls/"+n+"/merge", map[string]any{}); st != 200 {
		t.Fatalf("merge: %d %v", st, body)
	}
	_, _, open = call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if n := len(alertsOf(t, open)); n != 0 {
		t.Errorf("open alerts after the merged fix: %v", open)
	}
	_, _, fixed := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "fixed"})
	fl := alertsOf(t, fixed)
	if len(fl) != 1 || fl[0]["state"] != "fixed" || fl[0]["version"] != "v1.2.4" {
		t.Errorf("fixed: %v", fixed)
	}
	_, _, all := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "all"})
	if len(alertsOf(t, all)) != 1 {
		t.Errorf("all: %v", all)
	}
}

func TestAlertsDismissedAndRefusals(t *testing.T) {
	srv, call := alertForge(t)
	srv.Post("/_mock/repos/acme/shop/alerts/1/dismiss", nil)
	_, _, d := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "dismissed"})
	if l := alertsOf(t, d); len(l) != 1 || l[0]["state"] != "dismissed" {
		t.Errorf("dismissed: %v", d)
	}
	if isErr, msg, _ := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "ref": "other"}); !isErr || !strings.Contains(msg, "default branch") {
		t.Errorf("ref: %v %s", isErr, msg)
	}
	if isErr, _, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "ref": "main"}); isErr || out["ref"] != "main" {
		t.Errorf("default ref refused")
	}
	if isErr, _, _ := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "bogus"}); !isErr {
		t.Error("bad state accepted")
	}
	if isErr, _, _ := call("list_dependency_alerts", map[string]any{"project": "acme/none"}); !isErr {
		t.Error("missing project accepted")
	}
}

// alertPage makes n alerts that number from first.
func alertPage(first, n int) string {
	list := make([]string, 0, n)
	for i := first; i < first+n; i++ {
		list = append(list, `{"number":`+itoa(i)+`,"state":"open","html_url":"https://github.com/acme/shop/security/dependabot/`+itoa(i)+`",`+
			`"dependency":{"package":{"ecosystem":"npm","name":"lodash"},"manifest_path":"package-lock.json"},`+
			`"security_advisory":{"ghsa_id":"GHSA-c2f3-g4h5-j6m7","cve_id":null,"severity":"low","identifiers":[{"type":"GHSA","value":"GHSA-c2f3-g4h5-j6m7"}]},`+
			`"security_vulnerability":{"severity":"low","vulnerable_version_range":"< 4.17.21","first_patched_version":null}}`)
	}
	return "[" + strings.Join(list, ",") + "]"
}

const lockFile = `{"packages":{"node_modules/lodash":{"version":"4.17.20"}}}`

func lockJSON() string {
	return fileJSON("file", "base64", base64.StdEncoding.EncodeToString([]byte(lockFile)), len(lockFile))
}

func TestAlertsPagesByCursor(t *testing.T) {
	m, call := setup(t, map[string]handler{
		"GET " + repo: reply(200, allMethods[:len(allMethods)-1]+`,"default_branch":"main"}`),
		"GET " + repo + "/contents/package-lock.json": reply(200, lockJSON()),
		"GET " + alertsPath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			if r.URL.Query().Get("after") == "" {
				w.Header().Set("Link", `<https://api.github.com`+alertsPath+`?per_page=100&after=CURSOR2>; rel="next"`)
				_, _ = w.Write([]byte(alertPage(1, 2)))
				return
			}
			if r.URL.Query().Get("after") != "CURSOR2" || r.URL.Query().Get("state") != "open" {
				http.Error(w, `{"message":"bad query"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(alertPage(3, 1)))
		},
	})
	isErr, msg, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if isErr {
		t.Fatal(msg)
	}
	list := alertsOf(t, out)
	if len(list) != 3 || out["truncated"] != false {
		t.Fatalf("out = %v", out)
	}
	a := list[0]
	if a["version"] != "4.17.20" || a["purl"] != "pkg:npm/lodash@4.17.20" || a["severity"] != "low" || a["ecosystem"] != "npm" {
		t.Errorf("alert = %v", a)
	}
	// One read of the manifest serves the three alerts.
	if m.called("GET "+repo+"/contents/package-lock.json") != 1 || m.called("GET "+alertsPath) != 2 {
		t.Errorf("calls = %v", m.calls)
	}
}

func TestAlertsCutList(t *testing.T) {
	m, call := setup(t, map[string]handler{
		"GET " + repo: reply(200, allMethods[:len(allMethods)-1]+`,"default_branch":"main"}`),
		"GET " + alertsPath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			w.Header().Set("Link", `<https://api.github.com`+alertsPath+`?after=NEXT>; rel="next"`)
			_, _ = w.Write([]byte(alertPage(1, 100)))
		},
	})
	isErr, msg, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop"})
	if isErr {
		t.Fatal(msg)
	}
	if out["truncated"] != true || len(alertsOf(t, out)) != 1000 || m.called("GET "+alertsPath) != 10 {
		t.Errorf("truncated=%v n=%d calls=%d", out["truncated"], len(alertsOf(t, out)), m.called("GET "+alertsPath))
	}
}

func TestAlertsStateFilterAndToken(t *testing.T) {
	m, call := setup(t, map[string]handler{
		"GET " + repo: reply(200, allMethods[:len(allMethods)-1]+`,"default_branch":"main"}`),
		"GET " + alertsPath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			if r.URL.Query().Get("state") != "dismissed,auto_dismissed" {
				http.Error(w, `{"message":"bad state"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[{"number":9,"state":"auto_dismissed","dependency":{"package":{"ecosystem":"pip","name":"x"}},"security_advisory":{},"security_vulnerability":{}}]`))
		},
	})
	isErr, msg, out := call("list_dependency_alerts", map[string]any{"project": "acme/shop", "state": "dismissed"})
	if isErr {
		t.Fatal(msg)
	}
	if l := alertsOf(t, out); len(l) != 1 || l[0]["state"] != "dismissed" || l[0]["purl"] != nil {
		t.Errorf("out = %v", out)
	}
	if got := m.headers.Get("Authorization"); got != "Bearer ghp_secret" {
		t.Errorf("authorization = %q", got)
	}
}
