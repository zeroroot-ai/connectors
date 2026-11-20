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

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the GitHub REST API. A route is "METHOD escaped-path". The
// pages are https://docs.github.com/en/rest/pulls/pulls,
// https://docs.github.com/en/rest/checks/runs,
// https://docs.github.com/en/rest/commits/statuses,
// https://docs.github.com/en/rest/git/commits, https://docs.github.com/en/rest/git/refs,
// https://docs.github.com/en/rest/deployments/deployments and
// https://docs.github.com/en/rest/commits/commits#compare-two-commits.
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
		`"merge_commit_sha":` + nullable(merged, mergeSHA) + `,"mergeable":` + mergeable + `,"mergeable_state":"` + ms + `",` +
		`"commits":3,"head":{"sha":"` + head + `","ref":"fix"},"base":{"ref":"main","sha":"` + baseSHA + `"}}`
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
	return map[string]handler{
		"GET " + repo + "/commits/" + headSHA + "/check-runs": reply(200, `{"total_count":`+itoa(total)+`,"check_runs":[`+runs+`]}`),
		"GET " + repo + "/commits/" + headSHA + "/status":     reply(200, `{"state":"`+statusState+`","total_count":`+itoa(statusTotal)+`}`),
	}
}

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

func TestRefusalShowsInStateUntilTheHeadChanges(t *testing.T) {
	pr := openPR("blocked")
	routes := checks(greenRun, "success", 1)
	routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(pr)) }
	_, call := setup(t, routes)
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" {
		t.Fatalf("before a try: %v", out)
	}
	call("merge_merge_request", map[string]any{"merge_request": ref})
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "refused" || !strings.Contains(out["reason"].(string), "blocked") {
		t.Errorf("after the refusal: %v", out)
	}
	pr = strings.ReplaceAll(pr, headSHA, "dddddddddddddddddddddddddddddddddddddddd")
	routes["GET "+repo+"/commits/dddddddddddddddddddddddddddddddddddddddd/check-runs"] = reply(200, `{"total_count":0,"check_runs":[]}`)
	routes["GET "+repo+"/commits/dddddddddddddddddddddddddddddddddddddddd/status"] = reply(200, `{"state":"pending","total_count":0}`)
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" {
		t.Errorf("a new head clears the refusal: %v", out)
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
		`,"mergeable":true,"mergeable_state":"clean","head":{"sha":"` + revSHA + `","ref":"` + revertBranch + `"},"base":{"ref":"main"}}`
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
	return merge(routes, map[string]handler{
		"GET " + repo + "/commits/" + revSHA + "/check-runs": reply(200, `{"total_count":1,"check_runs":[`+revertChecks+`]}`),
		"GET " + repo + "/commits/" + revSHA + "/status":     reply(200, `{"state":"pending","total_count":0}`),
	})
}

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
func TestRevertRefusesARebaseMerge(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/git/commits/"+mergeSHA] = reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},"parents":[{"sha":"`+parentSHA+`"}]}`)
	compare := "GET " + repo + "/compare/" + baseSHA + "..." + mergeSHA
	routes[compare] = reply(200, `{"status":"ahead","ahead_by":3,"behind_by":0,"total_commits":3}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "rebase merge") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if m.called(compare) != 1 {
		t.Errorf("the tool did not compare the base with the merge commit: %v", m.calls)
	}
	for _, c := range []string{"POST " + repo + "/git/commits", "POST " + repo + "/git/refs", "POST " + repo + "/pulls"} {
		if m.called(c) != 0 {
			t.Errorf("a partial revert reached GitHub: %s", c)
		}
	}
}

func TestRevertOfASquashMerge(t *testing.T) {
	routes := revertRoutes(`[]`, greenRun)
	routes["GET "+repo+"/git/commits/"+mergeSHA] = reply(200, `{"sha":"`+mergeSHA+`","tree":{"sha":"tmerge"},"parents":[{"sha":"`+parentSHA+`"}]}`)
	compare := "GET " + repo + "/compare/" + baseSHA + "..." + mergeSHA
	routes[compare] = reply(200, `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1}`)
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if got := m.bodies["POST "+repo+"/git/commits"]["tree"]; got != "tparent" {
		t.Errorf("the revert tree is %v, want the tree of the parent", got)
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

func TestDeploymentQuery(t *testing.T) {
	var queries []string
	routes := deployRoutes(`[]`, `[]`, nil)
	routes["GET "+repo+"/deployments"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`[]`))
	}
	_, call := setup(t, routes)
	call("deployment_state", map[string]any{"merge_request": ref})
	joined := strings.Join(queries, " ")
	for _, want := range []string{"environment=production", "sha=" + mergeSHA} {
		if !strings.Contains(joined, want) {
			t.Errorf("queries %q lack %s", joined, want)
		}
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
	if len(tools) != 4 {
		t.Fatalf("tools = %d, want 4", len(tools))
	}
	for _, name := range []string{"merge_request_state", "deployment_state"} {
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
