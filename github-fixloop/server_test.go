package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the GitHub REST API. A route is "METHOD escaped-path". The
// pages are https://docs.github.com/en/rest/pulls/pulls,
// https://docs.github.com/en/rest/checks/runs,
// https://docs.github.com/en/rest/commits/statuses,
// https://docs.github.com/en/rest/git/commits, https://docs.github.com/en/rest/git/refs,
// https://docs.github.com/en/rest/deployments/deployments,
// https://docs.github.com/en/rest/commits/commits#compare-two-commits,
// https://docs.github.com/en/rest/repos/repos#get-a-repository,
// https://docs.github.com/en/rest/branches/branches#get-a-branch and
// https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch.
type handler = func(http.ResponseWriter, *http.Request, map[string]any)

type mock struct {
	mu      sync.Mutex
	routes  map[string]handler
	calls   []string
	bodies  map[string]map[string]any
	headers http.Header
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	m.calls = append(m.calls, key)
	m.headers = r.Header.Clone()
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	if m.bodies == nil {
		m.bodies = map[string]map[string]any{}
	}
	m.bodies[key] = body
	h, ok := m.routes[key]
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	h(w, r, body)
}

func (m *mock) called(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == key {
			n++
		}
	}
	return n
}

func reply(status int, body string) handler {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const (
	repo      = "/repos/acme/shop"
	headSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergeSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	parentSHA = "cccccccccccccccccccccccccccccccccccccccc"
	revSHA    = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	baseSHA   = "9999999999999999999999999999999999999999"
	pr7       = repo + "/pulls/7"
	ref       = "acme/shop#7"
)

func pullJSON(state string, merged bool, mergeable, ms, head string) string {
	return `{"number":7,"title":"Fix SQL injection","state":"` + state + `","merged":` + boolStr(merged) + `,` +
		`"html_url":"https://github.com/acme/shop/pull/7","merged_at":` + nullable(merged, "2026-11-20T09:00:00Z") + `,` +
		`"closed_at":` + nullable(state == "closed", "2026-11-20T07:00:00Z") + `,"updated_at":"2026-11-20T08:00:00Z",` +
		`"created_at":"2026-11-20T06:30:00Z",` +
		`"merge_commit_sha":` + nullable(merged, mergeSHA) + `,"mergeable":` + mergeable + `,"mergeable_state":"` + ms + `",` +
		`"commits":3,"head":{"sha":"` + head + `","ref":"fix","repo":{"full_name":"acme/shop"}},` +
		`"base":{"ref":"main","sha":"` + baseSHA + `","repo":{"full_name":"acme/shop"}}}`
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func nullable(ok bool, v string) string {
	if !ok {
		return "null"
	}
	return `"` + v + `"`
}

func openPR(ms string) string { return pullJSON("open", false, "true", ms, headSHA) }

var mergedPR = pullJSON("closed", true, "null", "unknown", headSHA)

func checks(runs, statusState string, statusTotal int) map[string]handler {
	total := strings.Count(runs, `"status"`)
	return merge(repoRules(), map[string]handler{
		"GET " + repo + "/commits/" + headSHA + "/check-runs": reply(200, `{"total_count":`+itoa(total)+`,"check_runs":[`+runs+`]}`),
		"GET " + repo + "/commits/" + headSHA + "/status":     reply(200, combinedJSON(statusState, statusTotal, 0, statusTotal)),
	})
}

// combinedJSON is one page of the combined status: n statuses, named
// ci/<first> to ci/<first+n-1>, of total statuses.
func combinedJSON(state string, total, first, n int) string {
	list := make([]string, 0, n)
	for i := range n {
		list = append(list, `{"context":"ci/`+itoa(first+i)+`","state":"`+state+`"}`)
	}
	return `{"state":"` + state + `","total_count":` + itoa(total) + `,"statuses":[` + strings.Join(list, ",") + `]}`
}

// repoRules answers the merge methods of the repository and the rules of the
// branch main: all merge methods allowed, no required check.
func repoRules() map[string]handler {
	return map[string]handler{
		"GET " + repo: reply(200, allMethods),
		"GET " + repo + "/branches/main": reply(200, `{"name":"main","protected":true,"protection":{"enabled":true,`+
			`"required_status_checks":{"enforcement_level":"non_admins","contexts":[],"checks":[]}}}`),
		"GET " + repo + "/rules/branches/main": reply(200, `[]`),
	}
}

const allMethods = `{"full_name":"acme/shop","allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":true}`

func itoa(n int) string { return strconv.Itoa(n) }

const (
	greenRun   = `{"name":"build","status":"completed","conclusion":"success"}`
	skippedRun = `{"name":"docs","status":"completed","conclusion":"skipped"}`
	redRun     = `{"name":"test","status":"completed","conclusion":"failure"}`
	queuedRun  = `{"name":"lint","status":"in_progress","conclusion":null}`
)

func merge(route, more map[string]handler) map[string]handler {
	for k, v := range more {
		route[k] = v
	}
	return route
}

func setup(t *testing.T, routes map[string]handler) (srv *mock, call func(string, map[string]any) (isErr bool, text string, out map[string]any)) {
	t.Helper()
	mergeableWait = 0
	m := &mock{routes: routes}
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITHUB_API_URL": ts.URL, "GITHUB_TOKEN": "ghp_secret", "GITHUB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string, map[string]any) {
		res := kittest.Call(t, cs, tool, args)
		var out map[string]any
		if !res.IsError {
			kittest.Structured(t, res, &out)
		}
		return res.IsError, kittest.Text(t, res), out
	}
}

func TestStateOpenAndHeaders(t *testing.T) {
	m, call := setup(t, merge(checks(greenRun+","+skippedRun, "pending", 0), map[string]handler{
		"GET " + pr7: reply(200, openPR("clean")),
	}))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if out["state"] != "open" || out["pipeline_green"] != true || out["pipeline_status"] != "success" ||
		out["at"] != "2026-11-20T08:00:00Z" || out["merge_request"] != ref || out["head_sha"] != headSHA {
		t.Errorf("out = %v", out)
	}
	if m.headers.Get("Authorization") != "Bearer ghp_secret" || m.headers.Get("X-GitHub-Api-Version") != "2022-11-28" ||
		m.headers.Get("Accept") != "application/vnd.github+json" {
		t.Errorf("headers = %v", m.headers)
	}
}

// The daemon links a pull request to a finding only when merge_request_state
// gives the source branch (head.ref) and the time of creation (created_at),
// in the format that the daemon parses (time.RFC3339). A pull request with no
// created_at gives no created_at, so the daemon refuses the link.
func TestStateSourceBranchAndCreatedAt(t *testing.T) {
	noTime := strings.Replace(openPR("clean"), `"created_at":"2026-11-20T06:30:00Z",`, "", 1)
	noTime = strings.Replace(noTime, `"number":7`, `"number":8`, 1)
	m, call := setup(t, merge(checks(greenRun, "pending", 0), map[string]handler{
		"GET " + pr7:               reply(200, openPR("clean")),
		"GET " + repo + "/pulls/8": reply(200, noTime),
	}))
	cases := []struct {
		pr, path, created string
	}{
		{ref, pr7, "2026-11-20T06:30:00Z"},
		{"acme/shop#8", repo + "/pulls/8", ""},
	}
	for _, c := range cases {
		isErr, text, out := call("merge_request_state", map[string]any{"merge_request": c.pr})
		if isErr {
			t.Fatalf("%s: %s", c.pr, text)
		}
		if m.called("GET "+c.path) != 1 {
			t.Errorf("%s: the tool did not read %s: %v", c.pr, c.path, m.calls)
		}
		if out["source_branch"] != "fix" {
			t.Errorf("%s: source_branch = %v, want fix", c.pr, out["source_branch"])
		}
		got, ok := out["created_at"]
		if c.created == "" && ok {
			t.Errorf("%s: created_at = %v, want no field", c.pr, got)
		}
		if c.created != "" && got != c.created {
			t.Errorf("%s: created_at = %v, want %q", c.pr, got, c.created)
		}
	}
}

// A fork can hold a branch with the name of a job branch. So a pull request
// from a fork, or from a fork that a person deleted (head.repo is null),
// gives no source_branch, and source_branch_reason says why. The daemon then
// refuses the link.
func TestStateFromAForkGivesNoSourceBranch(t *testing.T) {
	fork := strings.Replace(openPR("clean"), `"ref":"fix","repo":{"full_name":"acme/shop"}`,
		`"ref":"job/j-1","repo":{"full_name":"mallory/shop"}`, 1)
	deleted := strings.Replace(fork, `"repo":{"full_name":"mallory/shop"}`, `"repo":null`, 1)
	deleted = strings.Replace(deleted, `"number":7`, `"number":8`, 1)
	m, call := setup(t, merge(checks(greenRun, "pending", 0), map[string]handler{
		"GET " + pr7:               reply(200, fork),
		"GET " + repo + "/pulls/8": reply(200, deleted),
	}))
	for _, c := range []struct{ pr, path string }{{ref, pr7}, {"acme/shop#8", repo + "/pulls/8"}} {
		isErr, text, out := call("merge_request_state", map[string]any{"merge_request": c.pr})
		if isErr {
			t.Fatalf("%s: %s", c.pr, text)
		}
		if m.called("GET "+c.path) != 1 {
			t.Errorf("%s: the tool did not read %s: %v", c.pr, c.path, m.calls)
		}
		if b, ok := out["source_branch"]; ok {
			t.Errorf("%s: source_branch = %v, want no field", c.pr, b)
		}
		if out["source_branch_reason"] != fixloop.ForkReason {
			t.Errorf("%s: source_branch_reason = %v", c.pr, out["source_branch_reason"])
		}
		if out["created_at"] != "2026-11-20T06:30:00Z" || out["state"] != "open" {
			t.Errorf("%s: out = %v", c.pr, out)
		}
	}
}

// GitHub compares owner and repository names with no regard to case. So a
// head repository that differs from the base only in case is not a fork.
func TestStateRepoNameCaseIsNotAFork(t *testing.T) {
	pr := strings.Replace(openPR("clean"), `"ref":"fix","repo":{"full_name":"acme/shop"}`,
		`"ref":"fix","repo":{"full_name":"Acme/Shop"}`, 1)
	m, call := setup(t, merge(checks(greenRun, "pending", 0), map[string]handler{"GET " + pr7: reply(200, pr)}))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if m.called("GET "+pr7) != 1 {
		t.Errorf("the tool did not read the pull request: %v", m.calls)
	}
	if out["source_branch"] != "fix" || out["source_branch_reason"] != nil {
		t.Errorf("out = %v", out)
	}
}

// merge_merge_request refuses a pull request from a fork, or from a deleted
// fork, even when its checks passed. It never calls the merge API.
func TestMergeRefusesAFork(t *testing.T) {
	fork := strings.Replace(openPR("clean"), `"ref":"fix","repo":{"full_name":"acme/shop"}`,
		`"ref":"fix","repo":{"full_name":"mallory/shop"}`, 1)
	deleted := strings.Replace(fork, `"repo":{"full_name":"mallory/shop"}`, `"repo":null`, 1)
	for name, body := range map[string]string{"fork": fork, "deleted fork": deleted} {
		m, call := setup(t, merge(checks(greenRun, "pending", 0), map[string]handler{
			"GET " + pr7:            reply(200, body),
			"PUT " + pr7 + "/merge": reply(200, `{"merged":true}`),
		}))
		isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref, "sha": headSHA})
		if isErr {
			t.Fatalf("%s: %s", name, text)
		}
		if m.called("GET "+pr7) == 0 {
			t.Errorf("%s: the tool did not read the pull request: %v", name, m.calls)
		}
		if out["merged"] != false || out["state"] != fixloop.Refused || out["reason"] != fixloop.ForkReason {
			t.Errorf("%s: out = %v", name, out)
		}
		if m.called("PUT "+pr7+"/merge") != 0 {
			t.Errorf("%s: the tool merged a pull request from a fork", name)
		}
	}
}

// revert_merge_request refuses a merged pull request from a fork. It makes
// no commit, no branch and no revert pull request.
func TestRevertRefusesAFork(t *testing.T) {
	fork := strings.Replace(mergedPR, `"ref":"fix","repo":{"full_name":"acme/shop"}`,
		`"ref":"fix","repo":{"full_name":"mallory/shop"}`, 1)
	m, call := setup(t, map[string]handler{"GET " + pr7: reply(200, fork)})
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if out["merged"] != false || out["state"] != fixloop.Refused || out["reason"] != fixloop.ForkReason {
		t.Errorf("out = %v", out)
	}
	if len(m.calls) != 1 || m.called("GET "+pr7) != 1 {
		t.Errorf("the tool made more calls than the read of the pull request: %v", m.calls)
	}
}

func TestPipelineStatus(t *testing.T) {
	cases := []struct {
		name  string
		runs  string
		state string
		total int
		want  string
		green bool
	}{
		{"all green", greenRun, "success", 1, "success", true},
		{"red run", greenRun + "," + redRun, "success", 1, "failed", false},
		{"queued run", greenRun + "," + queuedRun, "success", 0, "pending", false},
		{"failed status", greenRun, "failure", 2, "failed", false},
		{"pending status", greenRun, "pending", 1, "pending", false},
		{"no checks", "", "pending", 0, "none", false},
		{"status only", "", "success", 1, "success", true},
	}
	for _, c := range cases {
		_, call := setup(t, merge(checks(c.runs, c.state, c.total), map[string]handler{"GET " + pr7: reply(200, openPR("clean"))}))
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		if out["pipeline_status"] != c.want || out["pipeline_green"] != c.green {
			t.Errorf("%s: out = %v", c.name, out)
		}
	}
}

func TestPipelineReadsEveryPage(t *testing.T) {
	page := 0
	routes := checks(greenRun, "pending", 0)
	routes["GET "+repo+"/commits/"+headSHA+"/check-runs"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		page++
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[` + greenRun + `]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[` + redRun + `]}`))
	}
	routes["GET "+pr7] = reply(200, openPR("clean"))
	_, call := setup(t, routes)
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if page != 2 || out["pipeline_status"] != "failed" {
		t.Errorf("pages = %d out = %v", page, out)
	}
}

// TestPipelineRefusesMoreRunsThanItReads asserts that a commit with more check
// runs than the tool reads is not green. Each page holds 100 green runs, and
// the runs after page 10 are never read.
func TestPipelineRefusesMoreRunsThanItReads(t *testing.T) {
	var pages []string
	routes := checks(greenRun, "success", 1)
	routes["GET "+repo+"/commits/"+headSHA+"/check-runs"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		pages = append(pages, r.URL.Query().Get("page")+"/"+r.URL.Query().Get("per_page"))
		_, _ = w.Write([]byte(`{"total_count":1001,"check_runs":[` + strings.TrimSuffix(strings.Repeat(greenRun+",", 100), ",") + `]}`))
	}
	routes["GET "+pr7] = reply(200, openPR("clean"))
	routes["PUT "+pr7+"/merge"] = reply(200, `{"merged":true}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("merge_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "1001 check runs") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if m.called("PUT "+pr7+"/merge") != 0 {
		t.Error("merged with check runs that the tool did not read")
	}
	if len(pages) != 10 || pages[0] != "1/100" || pages[9] != "10/100" {
		t.Errorf("pages = %v", pages)
	}
}

func TestStateMergedAndClosed(t *testing.T) {
	routes := checks(greenRun, "pending", 0)
	routes["GET "+pr7] = reply(200, mergedPR)
	routes["GET "+repo+"/pulls/8"] = reply(200, strings.Replace(pullJSON("closed", false, "null", "unknown", headSHA), `"number":7`, `"number":8`, 1))
	_, call := setup(t, routes)
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "merged" || out["merge_commit"] != mergeSHA || out["merged_at"] != "2026-11-20T09:00:00Z" || out["at"] != "2026-11-20T09:00:00Z" {
		t.Errorf("merged = %v", out)
	}
	_, _, out = call("merge_request_state", map[string]any{"merge_request": "acme/shop#8"})
	if out["state"] != "closed" || out["at"] != "2026-11-20T07:00:00Z" {
		t.Errorf("closed = %v", out)
	}
	isErr, text, _ := call("merge_request_state", map[string]any{"merge_request": "acme/shop#99"})
	if !isErr || !strings.Contains(text, "404") {
		t.Errorf("missing: %v %s", isErr, text)
	}
}

func TestParseRef(t *testing.T) {
	v := &service{host: "github.com"}
	good := map[string]prRef{
		"https://github.com/acme/shop/pull/7":       {"acme", "shop", 7},
		"https://github.com/acme/shop/pull/7/files": {"acme", "shop", 7},
		"https://GITHUB.com/acme/my.repo/pull/12":   {"acme", "my.repo", 12},
		"acme/shop#5": {"acme", "shop", 5},
	}
	for in, want := range good {
		got, err := v.parseRef(in)
		if err != nil || got != want {
			t.Errorf("%s -> %+v %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "7", "acme/shop!7", "acme/shop#x", "acme/shop#", "acme/../x#1", "acme/..#1",
		"https://evil.example.com/acme/shop/pull/7",
		"https://github.com/acme/shop/issues/7",
		"https://github.com/acme/shop/pull/7?x=1",
		"https://u:p@github.com/acme/shop/pull/7",
		"ftp://github.com/acme/shop/pull/7",
		"https://github.com/acme/shop/pull/0",
		"https://github.com/acme/../pull/7",
		"acme/shop#7\n", "a b/c#1",
	} {
		if _, err := v.parseRef(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestGitHubEnterpriseHost(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITHUB_API_URL": "https://ghe.example.com/api/v3", "GITHUB_TOKEN": "t", "GITHUB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil || register == nil {
		t.Fatal(err)
	}
	v := &service{host: "ghe.example.com"}
	if _, err := v.parseRef("https://ghe.example.com/acme/shop/pull/7"); err != nil {
		t.Error(err)
	}
}

func TestMergeSendsHeadSHA(t *testing.T) {
	merged := false
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		if merged {
			_, _ = w.Write([]byte(mergedPR))
			return
		}
		_, _ = w.Write([]byte(openPR("clean")))
	}
	routes["PUT "+pr7+"/merge"] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		merged = true
		_, _ = w.Write([]byte(`{"sha":"` + mergeSHA + `","merged":true,"message":"Pull Request successfully merged"}`))
	}
	m, call := setup(t, routes)
	isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref, "sha": headSHA, "merge_method": "squash"})
	if isErr {
		t.Fatal(text)
	}
	if out["merged"] != true || out["state"] != "merged" || out["merge_commit"] != mergeSHA {
		t.Errorf("out = %v", out)
	}
	body := m.bodies["PUT "+pr7+"/merge"]
	if body["sha"] != headSHA || body["merge_method"] != "squash" {
		t.Errorf("merge body = %v", body)
	}
}

func TestMergeNotAttempted(t *testing.T) {
	cases := []struct {
		name      string
		runs      string
		pr        string
		sha       string
		reason    string
		wantState string
	}{
		{"checks running", queuedRun, openPR("clean"), "", "have not succeeded: pending", "open"},
		{"checks failed", redRun, openPR("blocked"), "", "have not succeeded: failed", "open"},
		{"no checks", "", openPR("clean"), "", "have not succeeded: none", "open"},
		{"head moved", greenRun, openPR("clean"), "dddddddddddddddddddddddddddddddddddddddd", "A push changed it", "open"},
		{"unknown merge state", greenRun, pullJSON("open", false, "null", "unknown", headSHA), "", "has not settled", "open"},
		{"review missing", greenRun, openPR("blocked"), "", "blocked", "refused"},
		{"branch behind", greenRun, openPR("behind"), "", "behind", "refused"},
		{"conflict", greenRun, pullJSON("open", false, "false", "dirty", headSHA), "", "dirty", "refused"},
		{"draft", greenRun, openPR("draft"), "", "draft", "refused"},
		{"closed", greenRun, pullJSON("closed", false, "null", "unknown", headSHA), "", "is closed", "closed"},
	}
	for _, c := range cases {
		m, call := setup(t, merge(checks(c.runs, "pending", 0), map[string]handler{"GET " + pr7: reply(200, c.pr)}))
		args := map[string]any{"merge_request": ref}
		if c.sha != "" {
			args["sha"] = c.sha
		}
		isErr, text, out := call("merge_merge_request", args)
		reason, _ := out["reason"].(string)
		if isErr || out["merged"] != false || out["state"] != c.wantState || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: isErr=%v out=%v %s", c.name, isErr, out, text)
		}
		if m.called("PUT "+pr7+"/merge") != 0 {
			t.Errorf("%s: the tool called merge", c.name)
		}
	}
}

func TestMergeReadsAgainWhenMergeableIsNull(t *testing.T) {
	reads := 0
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reads++
		if reads == 1 {
			_, _ = w.Write([]byte(pullJSON("open", false, "null", "unknown", headSHA)))
			return
		}
		if reads == 2 {
			_, _ = w.Write([]byte(openPR("clean")))
			return
		}
		_, _ = w.Write([]byte(mergedPR))
	}
	routes["PUT "+pr7+"/merge"] = reply(200, `{"merged":true}`)
	_, call := setup(t, routes)
	isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Errorf("isErr=%v out=%v %s", isErr, out, text)
	}
}

func TestMergeRefusesAHeadThatMovedDuringTheWait(t *testing.T) {
	const moved = "dddddddddddddddddddddddddddddddddddddddd"
	reads := 0
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reads++
		if reads == 1 {
			_, _ = w.Write([]byte(pullJSON("open", false, "null", "unknown", headSHA)))
			return
		}
		// A push moved the head while GitHub computed the merge status.
		_, _ = w.Write([]byte(pullJSON("open", false, "true", "clean", moved)))
	}
	routes["PUT "+pr7+"/merge"] = reply(200, `{"merged":true}`)
	m, call := setup(t, routes)
	isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref})
	reason, _ := out["reason"].(string)
	if isErr || out["merged"] != false || out["head_sha"] != headSHA || !strings.Contains(reason, "head") {
		t.Errorf("isErr=%v out=%v %s", isErr, out, text)
	}
	if n := m.called("PUT " + pr7 + "/merge"); n != 0 {
		t.Errorf("the tool sent %d merge requests for a head that it did not check", n)
	}
}

// TestRefusalComesFromTheLiveState asserts that "refused" is the state of
// GitHub now. A fresh server, which stands for a second replica or a restart,
// sees the refusal with no merge attempt. When a person removes the block, the
// next read shows the pull request as open.
func TestRefusalComesFromTheLiveState(t *testing.T) {
	pr := openPR("blocked")
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(pr)) }
	m, call := setup(t, routes)
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "refused" || out["reason"] != "GitHub does not allow the merge: blocked" {
		t.Errorf("a blocked pull request with green checks: %v", out)
	}
	call("merge_merge_request", map[string]any{"merge_request": ref})
	if m.called("PUT "+pr7+"/merge") != 0 {
		t.Error("the tool called merge on a blocked pull request")
	}
	pr = openPR("clean") // a person approved the review
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" || out["reason"] != nil {
		t.Errorf("after a person removed the block: %v", out)
	}
}

func TestBlockedWithRunningChecksIsOpen(t *testing.T) {
	routes := checks(queuedRun, "pending", 0)
	routes["GET "+pr7] = reply(200, openPR("blocked"))
	_, call := setup(t, routes)
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" {
		t.Errorf("GitHub blocks a merge while required checks run. That is not a refusal: %v", out)
	}
}

func TestMergeVendorAnswers(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		after     string
		wantState string
		wantErr   bool
	}{
		{"head changed", 409, `{"message":"Head branch was modified. Review and try the merge again."}`, openPR("clean"), "open", false},
		{"not mergeable", 405, `{"message":"Required status check is expected."}`, openPR("clean"), "refused", false},
		{"no permission", 403, `{"message":"Resource not accessible by personal access token"}`, openPR("clean"), "refused", false},
		{"already merged by a retry", 405, `{"message":"Pull Request is not mergeable"}`, mergedPR, "merged", false},
		{"unauthorized", 401, `{"message":"Bad credentials"}`, openPR("clean"), "", true},
	}
	for _, c := range cases {
		reads := 0
		routes := checks(greenRun, "success", 1)
		routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			reads++
			if reads == 1 {
				_, _ = w.Write([]byte(openPR("clean")))
				return
			}
			_, _ = w.Write([]byte(c.after))
		}
		routes["PUT "+pr7+"/merge"] = reply(c.status, c.body)
		_, call := setup(t, routes)
		isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref})
		if isErr != c.wantErr {
			t.Errorf("%s: isErr=%v %s", c.name, isErr, text)
			continue
		}
		if !c.wantErr && out["state"] != c.wantState {
			t.Errorf("%s: out = %v", c.name, out)
		}
	}
}

func TestMergeAlreadyMergedIsNotAnError(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = reply(200, mergedPR)
	m, call := setup(t, routes)
	isErr, _, out := call("merge_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true || m.called("PUT "+pr7+"/merge") != 0 {
		t.Errorf("isErr=%v out=%v calls=%v", isErr, out, m.calls)
	}
}

const revertBranch = "zeroroot/revert-pr-7-bbbbbbbb"

func revertPR(state string, merged bool) string {
	return `{"number":8,"title":"Revert","state":"` + state + `","merged":` + boolStr(merged) + `,"html_url":"https://github.com/acme/shop/pull/8",` +
		`"merge_commit_sha":` + nullable(merged, "ffffffffffffffffffffffffffffffffffffffff") + `,"merged_at":` + nullable(merged, "2026-11-20T10:00:00Z") +
		`,"mergeable":true,"mergeable_state":"clean","head":{"sha":"` + revSHA + `","ref":"` + revertBranch + `",` +
		`"repo":{"full_name":"acme/shop"}},"base":{"ref":"main","repo":{"full_name":"acme/shop"}}}`
}

func revertRoutes(found, revertChecks string) map[string]handler {
	merged := false
	routes := map[string]handler{
		"GET " + pr7:                          reply(200, mergedPR),
		"GET " + repo + "/pulls":              reply(200, found),
		"GET " + repo + "/git/ref/heads/main": reply(200, `{"object":{"sha":"`+mergeSHA+`"}}`),
		"GET " + repo + "/git/ref/heads/zeroroot/revert-pr-7-bbbbbbbb": reply(404, `{"message":"Not Found"}`),
		"GET " + repo + "/git/commits/" + mergeSHA:                     reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},"parents":[{"sha":"`+parentSHA+`"},{"sha":"side"}]}`),
		"GET " + repo + "/git/commits/" + parentSHA:                    reply(200, `{"sha":"`+parentSHA+`","tree":{"sha":"tparent"},"parents":[]}`),
		"POST " + repo + "/git/commits":                                reply(201, `{"sha":"`+revSHA+`"}`),
		"POST " + repo + "/git/refs":                                   reply(201, `{}`),
		"POST " + repo + "/pulls":                                      reply(201, revertPR("open", false)),
		"GET " + repo + "/pulls/8": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			_, _ = w.Write([]byte(revertPR(map[bool]string{false: "open", true: "closed"}[merged], merged)))
		},
		"PUT " + repo + "/pulls/8/merge": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			merged = true
			_, _ = w.Write([]byte(`{"merged":true}`))
		},
	}
	return merge(merge(routes, repoRules()), map[string]handler{
		"GET " + repo + "/commits/" + revSHA + "/check-runs": reply(200, `{"total_count":1,"check_runs":[`+revertChecks+`]}`),
		"GET " + repo + "/commits/" + revSHA + "/status":     reply(200, `{"state":"pending","total_count":0}`),
		"GET " + repo + "/git/commits/" + revSHA: reply(200, `{"sha":"`+revSHA+`","tree":{"sha":"tparent"},`+
			`"parents":[{"sha":"`+mergeSHA+`"}]}`),
		"GET " + pr7 + "/commits": reply(200, prCommits),
		// The parent of a merge commit with one parent came from pull request 6.
		"GET " + repo + "/commits/" + parentSHA + "/pulls": reply(200, `[{"number":6,"state":"closed"}]`),
	})
}

// prCommits is the list of the three commits of pull request 7.
const prCommits = `[` +
	`{"sha":"1000000000000000000000000000000000000001","commit":{"message":"Add the query builder",` +
	`"author":{"name":"Ada","email":"ada@example.com","date":"2026-11-19T10:00:00Z"}}},` +
	`{"sha":"1000000000000000000000000000000000000002","commit":{"message":"Use bound parameters",` +
	`"author":{"name":"Ada","email":"ada@example.com","date":"2026-11-19T11:00:00Z"}}},` +
	`{"sha":"` + headSHA + `","commit":{"message":"Add a test",` +
	`"author":{"name":"Ada","email":"ada@example.com","date":"2026-11-19T12:00:00Z"}}}]`

func TestRevertBuildsTheCommitAndMerges(t *testing.T) {
	m, call := setup(t, revertRoutes(`[]`, greenRun))
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if out["merged"] != true || out["state"] != "merged" || out["revert_merge_request"] != "https://github.com/acme/shop/pull/8" {
		t.Errorf("out = %v", out)
	}
	commit := m.bodies["POST "+repo+"/git/commits"]
	if commit["tree"] != "tparent" {
		t.Errorf("the revert tree is %v, want the tree of the first parent", commit["tree"])
	}
	if p, _ := commit["parents"].([]any); len(p) != 1 || p[0] != mergeSHA {
		t.Errorf("parents = %v", commit["parents"])
	}
	if got := m.bodies["POST "+repo+"/git/refs"]; got["ref"] != "refs/heads/"+revertBranch || got["sha"] != revSHA {
		t.Errorf("ref body = %v", got)
	}
	pr := m.bodies["POST "+repo+"/pulls"]
	if pr["head"] != revertBranch || pr["base"] != "main" {
		t.Errorf("pull request body = %v", pr)
	}
	if got := m.bodies["PUT "+repo+"/pulls/8/merge"]["sha"]; got != revSHA {
		t.Errorf("merge sha = %v", got)
	}
	// A merge commit has two parents. It is not a squash merge or a rebase
	// merge, so the tool does not list the commits of the pull request.
	if m.called("GET "+pr7+"/commits") != 0 {
		t.Errorf("listed the commits of a merge with a merge commit: %v", m.calls)
	}
}

func TestRevertWaitsForTheChecks(t *testing.T) {
	m, call := setup(t, revertRoutes(`[]`, queuedRun))
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != false || out["state"] != "open" || out["pipeline_status"] != "pending" {
		t.Errorf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("PUT "+repo+"/pulls/8/merge") != 0 {
		t.Error("merged a revert with running checks")
	}
}

func TestRevertSecondCallFindsTheFirst(t *testing.T) {
	m, call := setup(t, revertRoutes(`[`+revertPR("open", false)+`]`, greenRun))
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	for _, c := range []string{"POST " + repo + "/git/commits", "POST " + repo + "/git/refs", "POST " + repo + "/pulls"} {
		if m.called(c) != 0 {
			t.Errorf("repeated %s", c)
		}
	}
}

// closedRevert answers the list of pull requests by the head branch: the
// closed branches have a closed revert pull request, the others have none.
func closedRevert(closed ...string) handler {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		head := r.URL.Query().Get("head")
		for _, b := range closed {
			if head == "acme:"+b {
				_, _ = w.Write([]byte(`[` + strings.Replace(revertPR("closed", false), `"merged":false`,
					`"merged":false,"closed_at":"2026-11-20T09:30:00Z"`, 1) + `]`))
				return
			}
		}
		_, _ = w.Write([]byte(`[]`))
	}
}

func TestRevertOpensANewOneAfterAPersonClosedIt(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/pulls"] = closedRevert(revertBranch)
	routes["GET "+repo+"/git/ref/heads/zeroroot/revert-pr-7-bbbbbbbb-2"] = reply(404, `{"message":"Not Found"}`)
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if got := m.bodies["POST "+repo+"/git/refs"]; got["ref"] != "refs/heads/"+revertBranch+"-2" {
		t.Errorf("ref body = %v", got)
	}
	if got := m.bodies["POST "+repo+"/pulls"]; got["head"] != revertBranch+"-2" || got["base"] != "main" {
		t.Errorf("pull request body = %v", got)
	}
}

func TestRevertStopsAfterThreeClosedReverts(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/pulls"] = closedRevert(revertBranch, revertBranch+"-2", revertBranch+"-3")
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "closed 3 revert") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if n := m.called("GET " + repo + "/pulls"); n != 3 {
		t.Errorf("looked for %d revert pull requests, want 3", n)
	}
	for _, c := range []string{"POST " + repo + "/git/commits", "POST " + repo + "/git/refs", "POST " + repo + "/pulls",
		"PUT " + repo + "/pulls/8/merge"} {
		if m.called(c) != 0 {
			t.Errorf("called %s after three closed reverts", c)
		}
	}
}

func TestRevertKeepsABranchFromAnEarlierCall(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/git/ref/heads/zeroroot/revert-pr-7-bbbbbbbb"] = reply(200, `{"object":{"sha":"`+revSHA+`"}}`)
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("POST "+repo+"/git/commits") != 0 || m.called("POST "+repo+"/git/refs") != 0 {
		t.Error("made the commit a second time")
	}
	if m.called("GET "+repo+"/git/commits/"+revSHA) != 1 {
		t.Errorf("the tool kept the branch without a check of its commit: %v", m.calls)
	}
}

func TestRevertRefusesWhenTheBranchMovedOn(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/git/ref/heads/main"] = reply(200, `{"object":{"sha":"1111111111111111111111111111111111111111"}}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "commits after the merge commit") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if m.called("POST "+repo+"/git/commits") != 0 {
		t.Error("made a commit that drops later work")
	}
}

// A merge commit with one parent comes from a squash merge or a rebase merge.
// After a rebase merge of three commits, the merge commit is the last of the
// three, and a revert of it undoes one third of the pull request
// (https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/about-merge-methods-on-github).
// The rebase gives the commits new shas and keeps the author, its date and the
// message. So the parent of the merge commit has a new sha and the author,
// the date and the message of the second commit of the pull request.
func TestRevertRefusesARebaseMerge(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/git/commits/"+mergeSHA] = reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},`+
		`"message":"Add a test","author":{"name":"Ada","email":"ada@example.com","date":"2026-11-19T12:00:00Z"},`+
		`"parents":[{"sha":"`+parentSHA+`"}]}`)
	routes["GET "+repo+"/git/commits/"+parentSHA] = reply(200, `{"sha":"`+parentSHA+`","tree":{"sha":"tparent"},`+
		`"message":"Use bound parameters\n","author":{"name":"Ada","email":"ada@example.com","date":"2026-11-19T11:00:00Z"},`+
		`"parents":[{"sha":"7777777777777777777777777777777777777777"}]}`)
	var pages []string
	routes["GET "+pr7+"/commits"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		pages = append(pages, r.URL.Query().Encode())
		_, _ = w.Write([]byte(prCommits))
	}
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "rebase merge of 3 commits") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if strings.Join(pages, " ") != "page=1&per_page=100" {
		t.Errorf("pages of the commit list = %v", pages)
	}
	for _, c := range []string{"POST " + repo + "/git/commits", "POST " + repo + "/git/refs", "POST " + repo + "/pulls"} {
		if m.called(c) != 0 {
			t.Errorf("a partial revert reached GitHub: %s", c)
		}
	}
}

// TestRevertOfASquashMergeAfterTheBaseMoved asserts that a squash merge is
// accepted when the base branch moved after the pull request was opened. The
// parent of the squash commit is the tip of the base before the merge, which
// is not a commit of the pull request.
func TestRevertOfASquashMergeAfterTheBaseMoved(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/git/commits/"+mergeSHA] = reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},`+
		`"message":"Fix SQL injection (#7)","author":{"name":"Ada","email":"ada@example.com","date":"2026-11-20T09:00:00Z"},`+
		`"parents":[{"sha":"`+parentSHA+`"}]}`)
	routes["GET "+repo+"/git/commits/"+parentSHA] = reply(200, `{"sha":"`+parentSHA+`","tree":{"sha":"tparent"},`+
		`"message":"Bump the version (#6)","author":{"name":"Bo","email":"bo@example.com","date":"2026-11-20T08:30:00Z"},`+
		`"parents":[{"sha":"`+baseSHA+`"}]}`)
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("GET "+pr7+"/commits") != 1 || m.called("GET "+repo+"/commits/"+parentSHA+"/pulls") != 1 {
		t.Errorf("the tool did not run both rebase checks: %v", m.calls)
	}
	if got := m.bodies["POST "+repo+"/git/commits"]["tree"]; got != "tparent" {
		t.Errorf("the revert tree is %v, want the tree of the parent", got)
	}
}

// TestRevertRefusesAPullRequestOfMoreThan250Commits asserts the limit of the
// commit list of GitHub. The tool cannot see every commit, so it refuses.
func TestRevertRefusesAPullRequestOfMoreThan250Commits(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+pr7] = reply(200, strings.Replace(mergedPR, `"commits":3`, `"commits":251`, 1))
	routes["GET "+repo+"/git/commits/"+mergeSHA] = reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},"parents":[{"sha":"`+parentSHA+`"}]}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "at most 250 commits") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if m.called("POST "+repo+"/git/commits") != 0 {
		t.Error("made a revert commit")
	}
}

// TestRevertRefusesABranchThatAPersonMade asserts that a branch with the name
// of the revert branch is kept only when its tip is the revert commit of the
// tool: one parent, the merge commit, and the tree of the parent.
func TestRevertRefusesABranchThatAPersonMade(t *testing.T) {
	const other = "4444444444444444444444444444444444444444"
	for name, tip := range map[string]string{
		"other tree":   `{"sha":"` + other + `","tree":{"sha":"tother"},"parents":[{"sha":"` + mergeSHA + `"}]}`,
		"other parent": `{"sha":"` + other + `","tree":{"sha":"tparent"},"parents":[{"sha":"` + parentSHA + `"}]}`,
		"two parents":  `{"sha":"` + other + `","tree":{"sha":"tparent"},"parents":[{"sha":"` + mergeSHA + `"},{"sha":"x"}]}`,
	} {
		routes := revertRoutes(`[]`, greenRun)
		routes["GET "+repo+"/git/ref/heads/zeroroot/revert-pr-7-bbbbbbbb"] = reply(200, `{"object":{"sha":"`+other+`"}}`)
		routes["GET "+repo+"/git/commits/"+other] = reply(200, tip)
		m, call := setup(t, routes)
		isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
		if !isErr || !strings.Contains(text, "not the revert commit of this tool") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		if m.called("GET "+repo+"/git/commits/"+other) != 1 {
			t.Errorf("%s: the tool did not read the tip of the branch: %v", name, m.calls)
		}
		for _, c := range []string{"POST " + repo + "/git/commits", "POST " + repo + "/git/refs", "POST " + repo + "/pulls"} {
			if m.called(c) != 0 {
				t.Errorf("%s: called %s", name, c)
			}
		}
	}
}

func TestRevertOfAnOpenPullRequest(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = reply(200, openPR("clean"))
	_, call := setup(t, routes)
	if isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref}); !isErr || !strings.Contains(text, "not merged") {
		t.Errorf("%v %s", isErr, text)
	}
}

func TestRevertBranchWithASlashInTheBaseName(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+pr7] = reply(200, strings.Replace(mergedPR, `"base":{"ref":"main",`, `"base":{"ref":"release/2026",`, 1))
	routes["GET "+repo+"/git/ref/heads/release/2026"] = reply(200, `{"object":{"sha":"`+mergeSHA+`"}}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if m.called("GET "+repo+"/git/ref/heads/release/2026") != 1 {
		t.Errorf("calls = %v", m.calls)
	}
}

func deployRoutes(exact, all string, statuses map[string]string) map[string]handler {
	routes := map[string]handler{
		"GET " + pr7: reply(200, mergedPR),
		"GET " + repo + "/deployments": func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			if r.URL.Query().Get("sha") != "" {
				_, _ = w.Write([]byte(exact))
				return
			}
			_, _ = w.Write([]byte(all))
		},
		"GET " + repo + "/compare/" + mergeSHA + "...1111111111111111111111111111111111111111": reply(200, `{"status":"ahead"}`),
		"GET " + repo + "/compare/" + mergeSHA + "...2222222222222222222222222222222222222222": reply(200, `{"status":"diverged"}`),
	}
	for id, body := range statuses {
		routes["GET "+repo+"/deployments/"+id+"/statuses"] = reply(200, body)
	}
	return routes
}

func TestDeploymentExactCommit(t *testing.T) {
	dep := `[{"id":41,"sha":"` + mergeSHA + `","created_at":"2026-11-20T09:02:00Z"}]`
	m, call := setup(t, deployRoutes(dep, dep, map[string]string{
		"41": `[{"state":"success","created_at":"2026-11-20T09:06:00Z"},{"state":"in_progress","created_at":"2026-11-20T09:03:00Z"}]`,
	}))
	isErr, text, out := call("deployment_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if out["deployed"] != true || out["deployment"] != "acme/shop/deployments/41" || out["application"] != "acme/shop" ||
		out["at"] != "2026-11-20T09:06:00Z" || out["environment"] != "production" {
		t.Errorf("out = %v", out)
	}
	if restart := out["restart"].(map[string]any); restart["needed"] != false {
		t.Errorf("restart = %v", restart)
	}
	if m.called("GET "+repo+"/compare/"+mergeSHA+"..."+mergeSHA) != 0 {
		t.Error("compared the commit with itself")
	}
}

func TestDeploymentOfALaterCommit(t *testing.T) {
	all := `[{"id":43,"sha":"1111111111111111111111111111111111111111","created_at":"2026-11-20T09:20:00Z"},` +
		`{"id":42,"sha":"1111111111111111111111111111111111111111","created_at":"2026-11-20T09:10:00Z"},` +
		`{"id":40,"sha":"2222222222222222222222222222222222222222","created_at":"2026-11-20T09:01:00Z"},` +
		`{"id":30,"sha":"3333333333333333333333333333333333333333","created_at":"2026-11-20T08:00:00Z"}]`
	_, call := setup(t, deployRoutes(`[]`, all, map[string]string{
		"40": `[{"state":"success","created_at":"2026-11-20T09:02:00Z"}]`,
		"42": `[{"state":"success","created_at":"2026-11-20T09:12:00Z"}]`,
		"43": `[{"state":"success","created_at":"2026-11-20T09:22:00Z"}]`,
	}))
	_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != true || out["deployment"] != "acme/shop/deployments/42" || out["at"] != "2026-11-20T09:12:00Z" {
		t.Errorf("out = %v (want the oldest deployment that holds the merge commit)", out)
	}
}

func TestDeploymentNotYet(t *testing.T) {
	dep := `[{"id":41,"sha":"` + mergeSHA + `","created_at":"2026-11-20T09:02:00Z"}]`
	for name, st := range map[string]string{
		"failure":     `[{"state":"failure","created_at":"2026-11-20T09:06:00Z"},{"state":"success","created_at":"2026-11-20T09:03:00Z"}]`,
		"in progress": `[{"state":"in_progress","created_at":"2026-11-20T09:06:00Z"}]`,
		"no status":   `[]`,
	} {
		_, call := setup(t, deployRoutes(dep, dep, map[string]string{"41": st}))
		_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
		if out["deployed"] != false || out["reason"] == "" {
			t.Errorf("%s: out = %v", name, out)
		}
	}
	_, call := setup(t, deployRoutes(`[]`, `[]`, nil))
	_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != false {
		t.Errorf("no deployment: %v", out)
	}
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = reply(200, openPR("clean"))
	_, call = setup(t, routes)
	_, _, out = call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != false {
		t.Errorf("open pull request: %v", out)
	}
}

// TestDeploymentRequests asserts each request that deployment_state sends:
// the two lists of deployments, the compare call for a later commit, and the
// statuses of the deployment that holds the merge commit.
func TestDeploymentRequests(t *testing.T) {
	const later = "1111111111111111111111111111111111111111"
	var got []string
	record := func(h handler) handler {
		return func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			got = append(got, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.Query().Encode())
			if body != nil {
				t.Errorf("%s %s has a body: %v", r.Method, r.URL.Path, body)
			}
			h(w, r, body)
		}
	}
	routes := deployRoutes(`[]`, `[{"id":42,"sha":"`+later+`","created_at":"2026-11-20T09:10:00Z"}]`,
		map[string]string{"42": `[{"state":"success","created_at":"2026-11-20T09:12:00Z"}]`})
	for _, k := range []string{"GET " + repo + "/deployments", "GET " + repo + "/compare/" + mergeSHA + "..." + later,
		"GET " + repo + "/deployments/42/statuses"} {
		routes[k] = record(routes[k])
	}
	_, call := setup(t, routes)
	_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != true {
		t.Fatalf("out = %v", out)
	}
	want := []string{
		"GET " + repo + "/deployments?environment=production&per_page=50&sha=" + mergeSHA,
		"GET " + repo + "/deployments?environment=production&per_page=50",
		"GET " + repo + "/compare/" + mergeSHA + "..." + later + "?per_page=1",
		"GET " + repo + "/deployments/42/statuses?per_page=30",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestInputValidationNeverReachesGitHub(t *testing.T) {
	m, call := setup(t, nil)
	for _, tool := range []string{"merge_request_state", "merge_merge_request", "revert_merge_request", "deployment_state"} {
		for _, bad := range []string{"", "acme/shop#7/../../x", "https://evil.example.com/a/b/pull/1", "a;b#1"} {
			if isErr, _, _ := call(tool, map[string]any{"merge_request": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
	}
	for _, args := range []map[string]any{
		{"merge_request": ref, "sha": "xyz"},
		{"merge_request": ref, "merge_method": "rebase"},
	} {
		if isErr, _, _ := call("merge_merge_request", args); !isErr {
			t.Errorf("accepted %v", args)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("reached GitHub: %v", m.calls)
	}
}

func TestAnnotations(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{"GITHUB_TOKEN": "t", "GITHUB_ENVIRONMENT": "production"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tools := kittest.Tools(t, kittest.Connect(t, register))
	if len(tools) != 7 {
		t.Fatalf("tools = %d, want 7", len(tools))
	}
	for _, name := range []string{"merge_request_state", "deployment_state", "read_file"} {
		if a := tools[name].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s must be read only", name)
		}
	}
	for _, name := range []string{"merge_merge_request", "revert_merge_request"} {
		tl := tools[name]
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || !a.IdempotentHint {
			t.Errorf("%s annotations = %+v", name, a)
		}
		if !strings.Contains(tl.Description, "Undo: call revert_merge_request") {
			t.Errorf("%s does not name its undo", name)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for name, cfg := range map[string]map[string]string{
		"no token":        {"GITHUB_ENVIRONMENT": "production"},
		"no environment":  {"GITHUB_TOKEN": "t"},
		"bad environment": {"GITHUB_TOKEN": "t", "GITHUB_ENVIRONMENT": "a b;c"},
		"bad url":         {"GITHUB_TOKEN": "t", "GITHUB_ENVIRONMENT": "production", "GITHUB_API_URL": "file:///etc"},
	} {
		_, err := build(kit.NewEnvFrom(cfg), slog.New(slog.DiscardHandler))
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestRequiredCheckThatHasNotStartedIsPending asserts that a blocked pull
// request is not refused while a check that a rule of the base branch
// requires has not reported. The check of another app starts later.
func TestRequiredCheckThatHasNotStartedIsPending(t *testing.T) {
	for name, rule := range map[string]map[string]handler{
		"branch protection": {"GET " + repo + "/branches/main": reply(200, `{"name":"main","protection":{"enabled":true,`+
			`"required_status_checks":{"contexts":["security-scan"],"checks":[{"context":"security-scan","app_id":null}]}}}`)},
		"ruleset": {"GET " + repo + "/rules/branches/main": reply(200, `[{"type":"pull_request"},`+
			`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,`+
			`"required_status_checks":[{"context":"security-scan","integration_id":42}]}}]`)},
	} {
		routes := merge(checks(greenRun, "success", 1), rule)
		routes["GET "+pr7] = reply(200, openPR("blocked"))
		m, call := setup(t, routes)
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		if out["state"] != "open" || out["pipeline_status"] != "pending" || out["pipeline_green"] != false ||
			out["reason"] != "these required checks have not reported: security-scan" {
			t.Errorf("%s: a required check that has not started: %v", name, out)
		}
		_, _, out = call("merge_merge_request", map[string]any{"merge_request": ref})
		if out["reason"] != "the checks have not succeeded: pending. these required checks have not reported: security-scan" ||
			m.called("PUT "+pr7+"/merge") != 0 {
			t.Errorf("%s: merge: %v", name, out)
		}
		if m.called("GET "+repo+"/branches/main") != 2 || m.called("GET "+repo+"/rules/branches/main") != 2 {
			t.Errorf("%s: calls = %v", name, m.calls)
		}
		// The required check reports as a commit status. The block now is a refusal.
		routes["GET "+repo+"/commits/"+headSHA+"/status"] = reply(200, `{"state":"success","total_count":1,`+
			`"statuses":[{"context":"security-scan","state":"success"}]}`)
		_, call = setup(t, routes)
		_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
		if out["state"] != "refused" || out["reason"] != "GitHub does not allow the merge: blocked" {
			t.Errorf("%s: after the required check passed: %v", name, out)
		}
	}
}

// TestRulesOf404MeansNoRules asserts that a GitHub Enterprise Server without
// rulesets, which answers 404 for the rules, does not break the state.
func TestRulesOf404MeansNoRules(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	delete(routes, "GET "+repo+"/rules/branches/main")
	routes["GET "+pr7] = reply(200, openPR("clean"))
	_, call := setup(t, routes)
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr || out["pipeline_green"] != true {
		t.Errorf("isErr=%v out=%v %s", isErr, out, text)
	}
}

// TestMergeUsesAMethodThatTheRepositoryAllows asserts the merge_method that
// reaches GitHub, and the stable refusal when no usable method is allowed.
func TestMergeUsesAMethodThatTheRepositoryAllows(t *testing.T) {
	cases := []struct {
		name, settings, want, method, reason string
	}{
		{"all allowed", allMethods, "merge", "", ""},
		{"squash only", `{"allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":true}`, "squash", "", ""},
		{"rebase only", `{"allow_merge_commit":false,"allow_squash_merge":false,"allow_rebase_merge":true}`, "", "", reasonNoMethod},
		{"merge asked, squash only", `{"allow_merge_commit":false,"allow_squash_merge":true}`, "", "merge", reasonMethodOff + "merge"},
	}
	for _, c := range cases {
		merged := false
		routes := checks(greenRun, "success", 1)
		routes["GET "+repo] = reply(200, c.settings)
		routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			if merged {
				_, _ = w.Write([]byte(mergedPR))
				return
			}
			_, _ = w.Write([]byte(openPR("clean")))
		}
		routes["PUT "+pr7+"/merge"] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			merged = true
			_, _ = w.Write([]byte(`{"merged":true}`))
		}
		m, call := setup(t, routes)
		args := map[string]any{"merge_request": ref}
		if c.method != "" {
			args["merge_method"] = c.method
		}
		isErr, text, out := call("merge_merge_request", args)
		if isErr {
			t.Errorf("%s: %s", c.name, text)
			continue
		}
		if c.reason == "" {
			if got := m.bodies["PUT "+pr7+"/merge"]["merge_method"]; got != c.want || out["merged"] != true {
				t.Errorf("%s: merge_method = %v out = %v", c.name, got, out)
			}
			continue
		}
		if out["state"] != "refused" || out["reason"] != c.reason || m.called("PUT "+pr7+"/merge") != 0 {
			t.Errorf("%s: out = %v calls = %v", c.name, out, m.calls)
		}
		if c.method != "" {
			continue
		}
		// The state tool reports the same refusal on each read.
		_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
		if out["state"] != "refused" || out["reason"] != c.reason {
			t.Errorf("%s: state = %v", c.name, out)
		}
	}
}

// TestMergeChecksTheHeadBeforeTheRefusal asserts that a moved head is the
// reason, also when GitHub blocks the merge.
func TestMergeChecksTheHeadBeforeTheRefusal(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = reply(200, openPR("blocked"))
	m, call := setup(t, routes)
	_, _, out := call("merge_merge_request", map[string]any{"merge_request": ref, "sha": "dddddddddddddddddddddddddddddddddddddddd"})
	reason, _ := out["reason"].(string)
	if !strings.Contains(reason, "A push changed it") || m.called("PUT "+pr7+"/merge") != 0 {
		t.Errorf("out = %v", out)
	}
}

// TestMergeRefusalReasonIsStable asserts that a refusal of the merge call
// gives the same reason on each call: the HTTP status and the message.
func TestMergeRefusalReasonIsStable(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = reply(200, openPR("clean"))
	routes["PUT "+pr7+"/merge"] = reply(405, `{"message":"Repository rule violations found"}`)
	_, call := setup(t, routes)
	for range 2 {
		_, _, out := call("merge_merge_request", map[string]any{"merge_request": ref})
		if out["state"] != "refused" || out["reason"] != "GitHub refused the merge (HTTP 405): Repository rule violations found" {
			t.Errorf("out = %v", out)
		}
	}
}

// rebaseParent sets a merge commit with one parent. The parent has a new sha,
// the message of the second commit of pull request 7, and an author date that
// differs from that commit. So the author date check does not see the rebase.
func rebaseParent(routes map[string]handler) {
	routes["GET "+repo+"/git/commits/"+mergeSHA] = reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},`+
		`"message":"Add a test","author":{"name":"Ada","email":"ada@example.com","date":"2026-11-20T09:00:00Z"},`+
		`"parents":[{"sha":"`+parentSHA+`"}]}`)
	routes["GET "+repo+"/git/commits/"+parentSHA] = reply(200, `{"sha":"`+parentSHA+`","tree":{"sha":"tparent"},`+
		`"message":"Use bound parameters","author":{"name":"Ada","email":"ada@example.com","date":"2026-11-20T09:00:00Z"},`+
		`"parents":[{"sha":"7777777777777777777777777777777777777777"}]}`)
}

// TestRevertRefusesARebaseMergeByThePullsOfTheParent asserts the second
// rebase check. GitHub lists pull request 7 as the pull request that
// introduced the parent of the merge commit, so this is a rebase merge.
func TestRevertRefusesARebaseMergeByThePullsOfTheParent(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	rebaseParent(routes)
	var queries []string
	routes["GET "+repo+"/commits/"+parentSHA+"/pulls"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		queries = append(queries, r.URL.Query().Encode())
		_, _ = w.Write([]byte(`[{"number":7,"state":"closed"}]`))
	}
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "rebase merge of 3 commits") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if strings.Join(queries, " ") != "per_page=100" {
		t.Errorf("queries of the pulls of the parent = %v", queries)
	}
	for _, c := range []string{"POST " + repo + "/git/commits", "POST " + repo + "/git/refs", "POST " + repo + "/pulls"} {
		if m.called(c) != 0 {
			t.Errorf("a partial revert reached GitHub: %s", c)
		}
	}
}

// TestRevertRefusesWhenThePullsOfTheParentFail asserts that the revert stops
// when GitHub does not answer the list of the pull requests of the parent.
func TestRevertRefusesWhenThePullsOfTheParentFail(t *testing.T) {
	for name, h := range map[string]handler{
		"server error": reply(500, `{"message":"Server Error"}`),
		"full page":    reply(200, `[`+strings.TrimSuffix(strings.Repeat(`{"number":6},`, 100), ",")+`]`),
	} {
		routes := revertRoutes(`[]`, greenRun)
		rebaseParent(routes)
		routes["GET "+repo+"/commits/"+parentSHA+"/pulls"] = h
		m, call := setup(t, routes)
		isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
		if !isErr || !strings.Contains(text, "cannot tell") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		if m.called("GET "+repo+"/commits/"+parentSHA+"/pulls") != 1 || m.called("POST "+repo+"/git/commits") != 0 {
			t.Errorf("%s: calls = %v", name, m.calls)
		}
	}
}

// TestHiddenMergeMethodsAreAnError asserts that a token that cannot read the
// merge methods gets an error that names the permission, not a refusal.
func TestHiddenMergeMethodsAreAnError(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	routes["GET "+repo] = reply(200, `{"full_name":"acme/shop"}`)
	routes["GET "+pr7] = reply(200, openPR("clean"))
	routes["PUT "+pr7+"/merge"] = reply(200, `{"merged":true}`)
	m, call := setup(t, routes)
	for _, tool := range []string{"merge_request_state", "merge_merge_request"} {
		isErr, text, _ := call(tool, map[string]any{"merge_request": ref})
		if !isErr || !strings.Contains(text, "contents:read and contents:write permissions") {
			t.Errorf("%s: isErr=%v %s", tool, isErr, text)
		}
	}
	if m.called("GET "+repo) != 2 || m.called("PUT "+pr7+"/merge") != 0 {
		t.Errorf("calls = %v", m.calls)
	}
}

// TestCombinedStatusReadsEveryPage asserts that the tool reads each page of
// the statuses. The required check reports on page 2.
func TestCombinedStatusReadsEveryPage(t *testing.T) {
	routes := merge(checks(greenRun, "success", 1), map[string]handler{
		"GET " + repo + "/branches/main": reply(200, `{"name":"main","protection":{"enabled":true,`+
			`"required_status_checks":{"contexts":["ci/150"],"checks":[]}}}`),
	})
	var pages []string
	routes["GET "+repo+"/commits/"+headSHA+"/status"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		q := r.URL.Query()
		pages = append(pages, q.Get("page")+"/"+q.Get("per_page"))
		if q.Get("page") == "1" {
			_, _ = w.Write([]byte(combinedJSON("success", 160, 0, 100)))
			return
		}
		_, _ = w.Write([]byte(combinedJSON("success", 160, 100, 60)))
	}
	routes["GET "+pr7] = reply(200, openPR("clean"))
	_, call := setup(t, routes)
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr || out["pipeline_status"] != "success" || out["reason"] != nil {
		t.Errorf("isErr=%v out=%v %s", isErr, out, text)
	}
	if strings.Join(pages, " ") != "1/100 2/100" {
		t.Errorf("pages = %v", pages)
	}
}

// TestCombinedStatusRefusesMoreStatusesThanItReads asserts that a commit with
// more statuses than the tool reads is an error, not a green commit.
func TestCombinedStatusRefusesMoreStatusesThanItReads(t *testing.T) {
	routes := checks(greenRun, "success", 1)
	var pages []string
	routes["GET "+repo+"/commits/"+headSHA+"/status"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		pages = append(pages, r.URL.Query().Get("page"))
		_, _ = w.Write([]byte(combinedJSON("success", 1001, 0, 100)))
	}
	routes["GET "+pr7] = reply(200, openPR("clean"))
	routes["PUT "+pr7+"/merge"] = reply(200, `{"merged":true}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("merge_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "1001 statuses") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if len(pages) != 10 || pages[9] != "10" || m.called("PUT "+pr7+"/merge") != 0 {
		t.Errorf("pages = %v calls = %v", pages, m.calls)
	}
}
