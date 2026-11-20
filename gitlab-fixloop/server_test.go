package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the GitLab REST API v4. A route is "METHOD escaped-path".
// The pages are https://docs.gitlab.com/api/merge_requests/,
// https://docs.gitlab.com/api/commits/#revert-a-commit,
// https://docs.gitlab.com/api/branches/, https://docs.gitlab.com/api/repositories/,
// https://docs.gitlab.com/api/deployments/, https://docs.gitlab.com/api/projects/
// and https://docs.gitlab.com/api/status_checks/.
type mock struct {
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request, body map[string]any)
	calls  []string
	bodies map[string]map[string]any
	token  string
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	m.calls = append(m.calls, key)
	m.token = r.Header.Get("PRIVATE-TOKEN")
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
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
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

func reply(status int, body string) func(http.ResponseWriter, *http.Request, map[string]any) {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const (
	api      = "/api/v4/projects/acme%2Fshop"
	mr7      = "/merge_requests/7"
	headSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergeSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func openMR(pipeline, detailed string) string {
	p := "null"
	if pipeline != "" {
		p = `{"id":1,"sha":"` + headSHA + `","status":"` + pipeline + `"}`
	}
	return `{"iid":7,"title":"Fix SQL injection","state":"opened","web_url":"https://gitlab.example.com/acme/shop/-/merge_requests/7",` +
		`"sha":"` + headSHA + `","merge_commit_sha":null,"target_branch":"main","source_branch":"fix",` +
		`"merge_status":"can_be_merged","detailed_merge_status":"` + detailed + `","head_pipeline":` + p + `,` +
		`"updated_at":"2026-11-20T08:00:00.000Z"}`
}

const mergedMR = `{"iid":7,"title":"Fix SQL injection","state":"merged","web_url":"https://gitlab.example.com/acme/shop/-/merge_requests/7",` +
	`"sha":"` + headSHA + `","merge_commit_sha":"` + mergeSHA + `","target_branch":"main","source_branch":"fix",` +
	`"merged_at":"2026-11-20T09:00:00.000Z","updated_at":"2026-11-20T09:00:00.000Z",` +
	`"head_pipeline":{"id":1,"sha":"` + headSHA + `","status":"success"}}`

func setup(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request, map[string]any)) (srv *mock, call func(string, map[string]any) (isErr bool, text string, out map[string]any)) {
	t.Helper()
	if routes == nil {
		routes = map[string]func(http.ResponseWriter, *http.Request, map[string]any){}
	}
	if _, ok := routes["GET "+api]; !ok {
		routes["GET "+api] = reply(200, projectMerge)
	}
	m := &mock{routes: routes}
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITLAB_BASE_URL": ts.URL, "GITLAB_TOKEN": "glpat-secret", "GITLAB_ENVIRONMENT": "production",
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

// projectMerge is a project that merges with a merge commit.
const projectMerge = `{"id":3,"path_with_namespace":"acme/shop","merge_method":"merge","squash_option":"default_off"}`

// The reference of the project on the mock host is the short form, because
// the mock listens on a random address.
const ref = "acme/shop!7"

func TestMergeRequestStateOpen(t *testing.T) {
	m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, openMR("success", "mergeable")),
	})
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if out["state"] != "open" || out["pipeline_green"] != true || out["pipeline_status"] != "success" ||
		out["at"] != "2026-11-20T08:00:00Z" || out["merge_request"] != ref {
		t.Errorf("out = %v", out)
	}
	if m.token != "glpat-secret" {
		t.Errorf("token header = %q", m.token)
	}
}

func TestMergeRequestStateMergedAndClosed(t *testing.T) {
	_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7:                 reply(200, mergedMR),
		"GET " + api + "/merge_requests/8": reply(200, `{"iid":8,"state":"closed","sha":"`+headSHA+`","closed_at":"2026-11-20T07:00:00Z"}`),
		"GET " + api + "/merge_requests/9": reply(200, `{"iid":9,"state":"opened","sha":"`+headSHA+`","head_pipeline":{"status":"failed"}}`),
	})
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "merged" || out["merge_commit"] != mergeSHA || out["merged_at"] != "2026-11-20T09:00:00Z" || out["at"] != "2026-11-20T09:00:00Z" {
		t.Errorf("merged = %v", out)
	}
	_, _, out = call("merge_request_state", map[string]any{"merge_request": "acme/shop!8"})
	if out["state"] != "closed" || out["at"] != "2026-11-20T07:00:00Z" {
		t.Errorf("closed = %v", out)
	}
	_, _, out = call("merge_request_state", map[string]any{"merge_request": "acme/shop!9"})
	if out["state"] != "open" || out["pipeline_green"] != false || out["pipeline_status"] != "failed" {
		t.Errorf("failed pipeline = %v", out)
	}
}

// A head pipeline counts only when it ran on the head commit. A pipeline with
// an empty or another sha tested other code.
func TestPipelineOfAnotherCommitIsNotGreen(t *testing.T) {
	for name, sha := range map[string]string{"empty sha": "", "other sha": "cccccccccccccccccccccccccccccccccccccccc"} {
		mr := strings.Replace(openMR("success", "mergeable"), `"id":1,"sha":"`+headSHA+`"`, `"id":1,"sha":"`+sha+`"`, 1)
		m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
			"GET " + api + mr7: reply(200, mr),
		})
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		if out["pipeline_green"] != false {
			t.Errorf("%s: state = %v", name, out)
		}
		_, _, out = call("merge_merge_request", map[string]any{"merge_request": ref})
		if out["merged"] != false || m.called("PUT "+api+mr7+"/merge") != 0 {
			t.Errorf("%s: merged on a pipeline of another commit: %v", name, out)
		}
	}
}

func TestMergeRequestStateNoPipelineAndMissing(t *testing.T) {
	_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, openMR("", "mergeable")),
	})
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["pipeline_green"] != false || out["pipeline_status"] != "none" {
		t.Errorf("out = %v", out)
	}
	isErr, text, _ := call("merge_request_state", map[string]any{"merge_request": "acme/shop!99"})
	if !isErr || !strings.Contains(text, "404") {
		t.Errorf("missing merge request: %v %s", isErr, text)
	}
}

func TestParseRef(t *testing.T) {
	v := &service{host: "gitlab.example.com"}
	good := map[string][2]any{
		"https://gitlab.example.com/acme/shop/-/merge_requests/7":             {"acme/shop", 7},
		"https://gitlab.example.com/acme/team/shop/-/merge_requests/12/diffs": {"acme/team/shop", 12},
		"https://GITLAB.example.com/acme/shop/merge_requests/3":               {"acme/shop", 3},
		"acme/team/shop!5": {"acme/team/shop", 5},
	}
	for in, want := range good {
		p, n, err := v.parseRef(in)
		if err != nil || p != want[0] || n != want[1] {
			t.Errorf("%s -> %q %d %v", in, p, n, err)
		}
	}
	for _, in := range []string{
		"", "7", "acme/shop#7", "acme/shop!x", "acme/shop!",
		"https://evil.example.com/acme/shop/-/merge_requests/7",
		"https://gitlab.example.com:8443/acme/shop/-/merge_requests/7",
		"https://gitlab.example.com/acme/shop/-/issues/7",
		"https://gitlab.example.com/acme/shop/-/merge_requests/7?x=1",
		"https://u:p@gitlab.example.com/acme/shop/-/merge_requests/7",
		"ftp://gitlab.example.com/acme/shop/-/merge_requests/7",
		"https://gitlab.example.com/../x/-/merge_requests/7",
		"acme/shop!7\n", "a b/c!1",
	} {
		if _, _, err := v.parseRef(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestMergeSendsHeadSHA(t *testing.T) {
	m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7:            reply(200, openMR("success", "mergeable")),
		"PUT " + api + mr7 + "/merge": reply(200, mergedMR),
	})
	isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref, "sha": headSHA})
	if isErr {
		t.Fatal(text)
	}
	if out["merged"] != true || out["state"] != "merged" || out["merge_commit"] != mergeSHA {
		t.Errorf("out = %v", out)
	}
	if got := m.bodies["PUT "+api+mr7+"/merge"]["sha"]; got != headSHA {
		t.Errorf("sha sent = %v", got)
	}
}

func TestMergeNotAttempted(t *testing.T) {
	cases := []struct {
		name      string
		mr        string
		sha       string
		reason    string
		wantState string
	}{
		{"pipeline running", openMR("running", "ci_still_running"), "", "has not succeeded: running", "open"},
		{"pipeline failed", openMR("failed", "ci_must_pass"), "", "has not succeeded: failed", "open"},
		{"no pipeline", openMR("", "mergeable"), "", "has not succeeded: none", "open"},
		{"head moved", openMR("success", "mergeable"), "cccccccccccccccccccccccccccccccccccccccc", "A push changed it", "open"},
		{"still checking", openMR("success", "checking"), "", "has not settled", "open"},
		{"approval missing", openMR("success", "not_approved"), "", "not_approved", "refused"},
		{"draft", openMR("success", "draft_status"), "", "draft_status", "refused"},
	}
	for _, c := range cases {
		m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
			"GET " + api + mr7: reply(200, c.mr),
		})
		args := map[string]any{"merge_request": ref}
		if c.sha != "" {
			args["sha"] = c.sha
		}
		isErr, text, out := call("merge_merge_request", args)
		if isErr || out["merged"] != false || out["state"] != c.wantState || !strings.Contains(out["reason"].(string), c.reason) {
			t.Errorf("%s: isErr=%v out=%v %s", c.name, isErr, out, text)
		}
		if m.called("PUT "+api+mr7+"/merge") != 0 {
			t.Errorf("%s: the tool called merge", c.name)
		}
	}
}

// TestRefusalComesFromTheLiveState asserts that "refused" is the state of
// GitLab now. A fresh server, which stands for a second replica or a restart,
// sees the refusal with no merge attempt. When a person removes the block, the
// next read shows the merge request as open.
func TestRefusalComesFromTheLiveState(t *testing.T) {
	mr := openMR("success", "not_approved")
	m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(mr)) },
	})
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "refused" || out["reason"] != "GitLab does not allow the merge: not_approved" {
		t.Errorf("a blocked merge request with a green pipeline: %v", out)
	}
	call("merge_merge_request", map[string]any{"merge_request": ref})
	if m.called("PUT "+api+mr7+"/merge") != 0 {
		t.Error("the tool called merge on a blocked merge request")
	}
	mr = openMR("success", "mergeable") // a person approved the merge request
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" || out["reason"] != nil {
		t.Errorf("after a person removed the block: %v", out)
	}
	mr = openMR("running", "not_approved")
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" {
		t.Errorf("a running pipeline is not a refusal: %v", out)
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
		merged    bool
	}{
		{"head changed", 409, `{"message":"SHA does not match HEAD of source branch"}`, openMR("success", "mergeable"), "open", false, false},
		{"branch protection", 405, `{"message":"405 Method Not Allowed"}`, openMR("success", "mergeable"), "refused", false, false},
		{"no permission", 403, `{"message":"403 Forbidden"}`, openMR("success", "mergeable"), "refused", false, false},
		{"already merged by a retry", 405, `{"message":"405 Method Not Allowed"}`, mergedMR, "merged", false, true},
		{"unauthorized", 401, `{"message":"401 Unauthorized"}`, openMR("success", "mergeable"), "", true, false},
	}
	for _, c := range cases {
		reads := 0
		_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
			"GET " + api + mr7: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				reads++
				if reads == 1 {
					_, _ = w.Write([]byte(openMR("success", "mergeable")))
					return
				}
				_, _ = w.Write([]byte(c.after))
			},
			"PUT " + api + mr7 + "/merge": reply(c.status, c.body),
		})
		isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref})
		if isErr != c.wantErr {
			t.Errorf("%s: isErr=%v %s", c.name, isErr, text)
			continue
		}
		if !c.wantErr && (out["state"] != c.wantState || out["merged"] != c.merged) {
			t.Errorf("%s: out = %v", c.name, out)
		}
	}
}

func TestMergeAlreadyMergedIsNotAnError(t *testing.T) {
	m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, mergedMR),
	})
	isErr, _, out := call("merge_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true || m.called("PUT "+api+mr7+"/merge") != 0 {
		t.Errorf("isErr=%v out=%v calls=%v", isErr, out, m.calls)
	}
}

const (
	revertBranch = "zeroroot%2Frevert-mr7-bbbbbbbb"
	revertMRJSON = `{"iid":8,"title":"Revert","state":"opened","web_url":"https://gitlab.example.com/acme/shop/-/merge_requests/8",` +
		`"sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","target_branch":"main","detailed_merge_status":"mergeable",` +
		`"head_pipeline":{"id":2,"sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","status":"success"}}`
)

func revertRoutes(found, revertMR string) map[string]func(http.ResponseWriter, *http.Request, map[string]any) {
	return map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7:                                            reply(200, mergedMR),
		"GET " + api + "/merge_requests":                              reply(200, found),
		"POST " + api + "/repository/branches":                        reply(201, `{"name":"x"}`),
		"POST " + api + "/repository/commits/" + mergeSHA + "/revert": reply(201, `{"id":"eeee"}`),
		"POST " + api + "/merge_requests":                             reply(201, `{"iid":8}`),
		"GET " + api + "/merge_requests/8":                            reply(200, revertMR),
		"PUT " + api + "/merge_requests/8/merge":                      reply(200, strings.Replace(revertMR, `"opened"`, `"merged"`, 1)),
	}
}

func TestRevertOpensAndMerges(t *testing.T) {
	m, call := setup(t, revertRoutes(`[]`, revertMRJSON))
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	if out["merged"] != true || out["state"] != "merged" ||
		out["revert_merge_request"] != "https://gitlab.example.com/acme/shop/-/merge_requests/8" {
		t.Errorf("out = %v", out)
	}
	if got := m.bodies["POST "+api+"/repository/commits/"+mergeSHA+"/revert"]["branch"]; got != "zeroroot/revert-mr7-bbbbbbbb" {
		t.Errorf("revert branch = %v", got)
	}
	mrBody := m.bodies["POST "+api+"/merge_requests"]
	if mrBody["source_branch"] != "zeroroot/revert-mr7-bbbbbbbb" || mrBody["target_branch"] != "main" {
		t.Errorf("merge request body = %v", mrBody)
	}
	if got := m.bodies["PUT "+api+"/merge_requests/8/merge"]["sha"]; got != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Errorf("merge sha = %v", got)
	}
}

func TestRevertWaitsForThePipeline(t *testing.T) {
	pending := strings.Replace(revertMRJSON, `"status":"success"`, `"status":"running"`, 1)
	m, call := setup(t, revertRoutes(`[]`, pending))
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != false || out["state"] != "open" || out["pipeline_status"] != "running" {
		t.Errorf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("PUT "+api+"/merge_requests/8/merge") != 0 {
		t.Error("merged a revert with a running pipeline")
	}
}

func TestRevertSecondCallFindsTheFirst(t *testing.T) {
	found := `[{"iid":8}]`
	m, call := setup(t, revertRoutes(found, revertMRJSON))
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	for _, c := range []string{"POST " + api + "/repository/branches", "POST " + api + "/merge_requests",
		"POST " + api + "/repository/commits/" + mergeSHA + "/revert"} {
		if m.called(c) != 0 {
			t.Errorf("repeated %s", c)
		}
	}
}

// closedReverts answers the list of merge requests by the source branch: the
// closed branches have a closed revert merge request, the others have none.
func closedReverts(closed ...string) func(http.ResponseWriter, *http.Request, map[string]any) {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		for _, b := range closed {
			if r.URL.Query().Get("source_branch") == b {
				_, _ = w.Write([]byte(`[{"iid":5,"state":"closed","source_branch":"` + b + `"}]`))
				return
			}
		}
		_, _ = w.Write([]byte(`[]`))
	}
}

func TestRevertOpensANewOneAfterAPersonClosedIt(t *testing.T) {
	const first = "zeroroot/revert-mr7-bbbbbbbb"
	routes := revertRoutes(`[]`, revertMRJSON)
	routes["GET "+api+"/merge_requests"] = closedReverts(first)
	var branchQuery url.Values
	routes["POST "+api+"/repository/branches"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		branchQuery = r.URL.Query()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"x"}`))
	}
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if branchQuery.Get("branch") != first+"-2" || branchQuery.Get("ref") != "main" {
		t.Errorf("branch query = %v", branchQuery)
	}
	if got := m.bodies["POST "+api+"/repository/commits/"+mergeSHA+"/revert"]["branch"]; got != first+"-2" {
		t.Errorf("revert branch = %v", got)
	}
	if got := m.bodies["POST "+api+"/merge_requests"]; got["source_branch"] != first+"-2" || got["target_branch"] != "main" {
		t.Errorf("merge request body = %v", got)
	}
}

func TestRevertStopsAfterThreeClosedReverts(t *testing.T) {
	const first = "zeroroot/revert-mr7-bbbbbbbb"
	routes := revertRoutes(`[]`, revertMRJSON)
	routes["GET "+api+"/merge_requests"] = closedReverts(first, first+"-2", first+"-3")
	m, call := setup(t, routes)
	isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
	if !isErr || !strings.Contains(text, "closed 3 revert") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if n := m.called("GET " + api + "/merge_requests"); n != 3 {
		t.Errorf("looked for %d revert merge requests, want 3", n)
	}
	for _, c := range []string{"POST " + api + "/repository/branches", "POST " + api + "/merge_requests",
		"POST " + api + "/repository/commits/" + mergeSHA + "/revert", "PUT " + api + "/merge_requests/8/merge"} {
		if m.called(c) != 0 {
			t.Errorf("called %s after three closed reverts", c)
		}
	}
}

const (
	revertSHA   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	firstParent = "cccccccccccccccccccccccccccccccccccccccc"
	laterTarget = "dddddddddddddddddddddddddddddddddddddddd"
	// mergeChange is the compare of the merge commit to its first parent: the
	// change that a revert makes.
	mergeChange  = "@@ -10,3 +10,3 @@ func query() {\n ctx := context()\n-db.Query(sql, args...)\n+db.Query(raw)\n }\n"
	revertChange = "@@ -14,3 +14,3 @@ func query() {\n log := logger()\n-db.Query(sql, args...)\n+db.Query(raw)\n }\n"
)

// existingBranch answers the routes of a revert branch that exists already:
// one commit after the target branch, its parent, and the two compares that
// the tool reads. revertDiff is the change of the commit on the branch.
func existingBranch(routes map[string]func(http.ResponseWriter, *http.Request, map[string]any), branchCommits, revertParent, revertDiff string) {
	routes["POST "+api+"/repository/branches"] = reply(400, `{"message":"Branch already exists"}`)
	routes["GET "+api+"/repository/commits/"+revertSHA] = reply(200, `{"id":"`+revertSHA+`","parent_ids":["`+revertParent+`"]}`)
	routes["GET "+api+"/repository/commits/"+mergeSHA] = reply(200, `{"id":"`+mergeSHA+`","parent_ids":["`+firstParent+`","ffff"]}`)
	routes["GET "+api+"/repository/merge_base"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		refs := r.URL.Query()["refs[]"]
		if len(refs) == 2 && refs[0] == mergeSHA && refs[1] == laterTarget {
			_, _ = w.Write([]byte(`{"id":"` + mergeSHA + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"0000"}`))
	}
	routes["GET "+api+"/repository/compare"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		q := r.URL.Query()
		switch {
		case q.Get("from") == "main" && q.Get("to") == "zeroroot/revert-mr7-bbbbbbbb" && q.Get("straight") == "":
			_, _ = w.Write([]byte(`{"commits":` + branchCommits + `}`))
		case q.Get("from") == mergeSHA && q.Get("to") == firstParent && q.Get("straight") == "true":
			_, _ = w.Write([]byte(`{"diffs":[{"old_path":"db.go","new_path":"db.go","diff":` + jsonString(mergeChange) + `}]}`))
		case q.Get("from") == revertParent && q.Get("to") == revertSHA && q.Get("straight") == "true":
			_, _ = w.Write([]byte(`{"diffs":[{"old_path":"db.go","new_path":"db.go","diff":` + jsonString(revertDiff) + `}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestRevertKeepsABranchThatHoldsTheRevert asserts that the tool keeps a
// branch from an earlier call. The target branch moved on before the revert,
// so the line numbers and the context lines differ from the merge commit.
func TestRevertKeepsABranchThatHoldsTheRevert(t *testing.T) {
	routes := revertRoutes(`[]`, revertMRJSON)
	existingBranch(routes, `[{"id":"`+revertSHA+`"}]`, laterTarget, revertChange)
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("POST "+api+"/repository/commits/"+mergeSHA+"/revert") != 0 {
		t.Error("reverted a second time")
	}
	if m.called("GET "+api+"/repository/commits/"+revertSHA) != 1 || m.called("GET "+api+"/repository/compare") != 3 {
		t.Errorf("the tool kept the branch without a check of its commit: %v", m.calls)
	}
}

// TestRevertRefusesABranchThatAPersonMade asserts that a branch with the name
// of the revert branch is refused when its commit is not the revert.
func TestRevertRefusesABranchThatAPersonMade(t *testing.T) {
	cases := []struct {
		name, commits, parent, diff, want string
	}{
		{"other change", `[{"id":"` + revertSHA + `"}]`, laterTarget,
			"@@ -14,3 +14,3 @@\n-db.Query(sql, args...)\n+db.Exec(raw)\n", "does not revert the merge commit"},
		{"parent without the merge", `[{"id":"` + revertSHA + `"}]`, "9999999999999999999999999999999999999999",
			revertChange, "does not revert the merge commit"},
		{"two commits", `[{"id":"` + revertSHA + `"},{"id":"1234"}]`, laterTarget, revertChange, "has 2 commits"},
	}
	for _, c := range cases {
		routes := revertRoutes(`[]`, revertMRJSON)
		existingBranch(routes, c.commits, c.parent, c.diff)
		m, call := setup(t, routes)
		isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref})
		if !isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s: isErr=%v %s", c.name, isErr, text)
		}
		for _, k := range []string{"POST " + api + "/repository/commits/" + mergeSHA + "/revert", "POST " + api + "/merge_requests"} {
			if m.called(k) != 0 {
				t.Errorf("%s: called %s", c.name, k)
			}
		}
	}
}

func TestRevertErrors(t *testing.T) {
	// not merged
	_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, openMR("success", "mergeable")),
	})
	if isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref}); !isErr || !strings.Contains(text, "not merged") {
		t.Errorf("open merge request: %v %s", isErr, text)
	}
	// fast-forward merge without a merge commit
	ff := strings.Replace(mergedMR, `"merge_commit_sha":"`+mergeSHA+`"`, `"merge_commit_sha":null`, 1)
	_, call = setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, ff),
	})
	if isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref}); !isErr || !strings.Contains(text, "fast-forward") {
		t.Errorf("fast-forward: %v %s", isErr, text)
	}
	// GitLab cannot revert the commit
	routes := revertRoutes(`[]`, revertMRJSON)
	routes["POST "+api+"/repository/commits/"+mergeSHA+"/revert"] = reply(400, `{"message":"Sorry, we cannot revert this commit automatically."}`)
	_, call = setup(t, routes)
	if isErr, text, _ := call("revert_merge_request", map[string]any{"merge_request": ref}); !isErr || !strings.Contains(text, "cannot revert") {
		t.Errorf("revert conflict: %v %s", isErr, text)
	}
}

func deployments(list string) map[string]func(http.ResponseWriter, *http.Request, map[string]any) {
	return map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7:            reply(200, mergedMR),
		"GET " + api + "/deployments": reply(200, list),
		"GET " + api + "/repository/merge_base": func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			refs := r.URL.Query()["refs[]"]
			if len(refs) == 2 && refs[1] == "1111111111111111111111111111111111111111" {
				_, _ = w.Write([]byte(`{"id":"` + mergeSHA + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"0000"}`))
		},
	}
}

func TestDeploymentExactCommit(t *testing.T) {
	m, call := setup(t, deployments(`[{"id":41,"sha":"`+mergeSHA+`","status":"success","updated_at":"2026-11-20T09:05:00Z","finished_at":"2026-11-20T09:06:00Z"}]`))
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
	if m.called("GET "+api+"/repository/merge_base") != 0 {
		t.Error("asked for a merge base when the sha matched")
	}
}

func TestDeploymentOfALaterCommit(t *testing.T) {
	routes := deployments(`[` +
		`{"id":40,"sha":"2222222222222222222222222222222222222222","status":"success","updated_at":"2026-11-20T09:01:00Z"},` +
		`{"id":42,"sha":"1111111111111111111111111111111111111111","status":"success","updated_at":"2026-11-20T09:10:00Z"}]`)
	var bases []string
	mergeBase := routes["GET "+api+"/repository/merge_base"]
	routes["GET "+api+"/repository/merge_base"] = func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		bases = append(bases, r.Method+" "+strings.Join(r.URL.Query()["refs[]"], ","))
		mergeBase(w, r, body)
	}
	_, call := setup(t, routes)
	_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != true || out["deployment"] != "acme/shop/deployments/42" || out["at"] != "2026-11-20T09:10:00Z" {
		t.Errorf("out = %v", out)
	}
	want := []string{
		"GET " + mergeSHA + ",2222222222222222222222222222222222222222",
		"GET " + mergeSHA + ",1111111111111111111111111111111111111111",
	}
	if strings.Join(bases, " ") != strings.Join(want, " ") {
		t.Errorf("merge base requests = %v, want %v", bases, want)
	}
}

func TestDeploymentNotYet(t *testing.T) {
	m, call := setup(t, deployments(`[{"id":40,"sha":"2222222222222222222222222222222222222222","status":"success","updated_at":"2026-11-20T09:01:00Z"}]`))
	_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != false || out["reason"] == "" {
		t.Errorf("out = %v", out)
	}
	if !strings.Contains(strings.Join(m.calls, " "), "deployments") {
		t.Error("the tool did not list deployments")
	}
	// not merged yet
	_, call = setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, openMR("success", "mergeable")),
	})
	_, _, out = call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != false {
		t.Errorf("open merge request: %v", out)
	}
}

// TestDeploymentQuery asserts the whole query of the deployment list. GitLab
// answers 400 when updated_after comes without order_by=updated_at
// (https://docs.gitlab.com/api/deployments/#list-project-deployments).
func TestDeploymentQuery(t *testing.T) {
	var got url.Values
	var method string
	routes := deployments(`[]`)
	routes["GET "+api+"/deployments"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		got, method = r.URL.Query(), r.Method
		if got.Has("updated_after") && got.Get("order_by") != "updated_at" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"400 Bad request - order_by must be updated_at with updated_after"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}
	_, call := setup(t, routes)
	isErr, text, _ := call("deployment_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	want := url.Values{
		"environment": {"production"}, "status": {"success"}, "order_by": {"updated_at"}, "sort": {"asc"},
		"per_page": {"50"}, "updated_after": {"2026-11-20T09:00:00Z"},
	}
	if method != http.MethodGet || got.Encode() != want.Encode() {
		t.Errorf("request = %s ?%s, want GET ?%s", method, got.Encode(), want.Encode())
	}
}

func TestInputValidationNeverReachesGitLab(t *testing.T) {
	m, call := setup(t, nil)
	for _, tool := range []string{"merge_request_state", "merge_merge_request", "revert_merge_request", "deployment_state"} {
		for _, bad := range []string{"", "acme/shop!7/../../x", "https://evil.example.com/a/b/-/merge_requests/1", "a;b!1"} {
			if isErr, _, _ := call(tool, map[string]any{"merge_request": bad}); !isErr {
				t.Errorf("%s accepted %q", tool, bad)
			}
		}
	}
	if isErr, _, _ := call("merge_merge_request", map[string]any{"merge_request": ref, "sha": "xyz"}); !isErr {
		t.Error("accepted a bad sha")
	}
	if len(m.calls) != 0 {
		t.Errorf("reached GitLab: %v", m.calls)
	}
}

func TestAnnotations(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITLAB_BASE_URL": "https://gitlab.example.com", "GITLAB_TOKEN": "t", "GITLAB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
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
	for name, undo := range map[string]string{"merge_merge_request": "revert_merge_request", "revert_merge_request": "revert_merge_request"} {
		tl := tools[name]
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || !a.IdempotentHint {
			t.Errorf("%s annotations = %+v", name, a)
		}
		if !strings.Contains(tl.Description, "Undo: call "+undo) {
			t.Errorf("%s does not name its undo %s", name, undo)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for name, cfg := range map[string]map[string]string{
		"no url":          {"GITLAB_TOKEN": "t", "GITLAB_ENVIRONMENT": "production"},
		"no token":        {"GITLAB_BASE_URL": "https://g", "GITLAB_ENVIRONMENT": "production"},
		"no environment":  {"GITLAB_BASE_URL": "https://g", "GITLAB_TOKEN": "t"},
		"bad environment": {"GITLAB_BASE_URL": "https://g", "GITLAB_TOKEN": "t", "GITLAB_ENVIRONMENT": "a b;c"},
		"bad url":         {"GITLAB_BASE_URL": "file:///etc", "GITLAB_TOKEN": "t", "GITLAB_ENVIRONMENT": "production"},
	} {
		_, err := build(kit.NewEnvFrom(cfg), slog.New(slog.DiscardHandler))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "glpat") {
			t.Errorf("%s: error shows a secret", name)
		}
	}
}

var _ = fixloop.Open

// TestExternalStatusChecks asserts the map of status_checks_must_pass: the
// tool reads the status checks, and a pending check is a pending pipeline, not
// a refusal. A failed check is a failed pipeline.
func TestExternalStatusChecks(t *testing.T) {
	cases := []struct {
		detailed, checks, wantStatus, wantState string
		green                                   bool
	}{
		{"status_checks_must_pass", `[{"id":1,"name":"scan","status":"pending"},{"id":2,"name":"sbom","status":"passed"}]`,
			"pending", "open", false},
		{"external_status_checks", `[{"id":1,"name":"scan","status":"pending"}]`, "pending", "open", false},
		{"status_checks_must_pass", `[{"id":1,"name":"scan","status":"failed"},{"id":2,"name":"sbom","status":"pending"}]`,
			"failed", "open", false},
		{"status_checks_must_pass", `[{"id":1,"name":"scan","status":"passed"}]`, "success", "refused", true},
	}
	for _, c := range cases {
		m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
			"GET " + api + mr7:                    reply(200, openMR("success", c.detailed)),
			"GET " + api + mr7 + "/status_checks": reply(200, c.checks),
		})
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		if out["state"] != c.wantState || out["pipeline_status"] != c.wantStatus || out["pipeline_green"] != c.green {
			t.Errorf("%s %s: out = %v", c.detailed, c.checks, out)
		}
		if m.called("GET "+api+mr7+"/status_checks") != 1 {
			t.Errorf("%s: calls = %v", c.detailed, m.calls)
		}
	}
}

// TestMergeTimeIsNotARefusal asserts that merge_time, "May not be merged
// until after the specified time", waits and is not a refusal.
func TestMergeTimeIsNotARefusal(t *testing.T) {
	m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, openMR("success", "merge_time")),
	})
	_, _, out := call("merge_merge_request", map[string]any{"merge_request": ref})
	reason, _ := out["reason"].(string)
	if out["state"] != "open" || !strings.Contains(reason, "has not settled the merge status: merge_time") {
		t.Errorf("out = %v", out)
	}
	if m.called("PUT "+api+mr7+"/merge") != 0 {
		t.Error("the tool called merge before the merge time")
	}
}

// TestFastForwardWithoutSquashIsRefused asserts that a project that merges by
// fast-forward is refused unless the merge request squashes, because such a
// merge leaves no commit that the revert tool can revert.
func TestFastForwardWithoutSquashIsRefused(t *testing.T) {
	const ff = `{"id":3,"merge_method":"ff"}`
	squash := strings.Replace(openMR("success", "mergeable"), `"sha":`, `"squash_on_merge":true,"sha":`, 1)
	cases := []struct {
		name, project, mr string
		merge             bool
	}{
		{"ff without squash", ff, openMR("success", "mergeable"), false},
		{"ff with squash", ff, squash, true},
		{"merge commit", projectMerge, openMR("success", "mergeable"), true},
		{"rebase_merge", `{"id":3,"merge_method":"rebase_merge"}`, openMR("success", "mergeable"), true},
	}
	for _, c := range cases {
		m, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
			"GET " + api:                  reply(200, c.project),
			"GET " + api + mr7:            reply(200, c.mr),
			"PUT " + api + mr7 + "/merge": reply(200, mergedMR),
		})
		_, _, out := call("merge_merge_request", map[string]any{"merge_request": ref})
		if c.merge {
			if out["merged"] != true || m.called("PUT "+api+mr7+"/merge") != 1 {
				t.Errorf("%s: out = %v", c.name, out)
			}
			continue
		}
		if out["state"] != "refused" || out["reason"] != reasonFastForward || m.called("PUT "+api+mr7+"/merge") != 0 {
			t.Errorf("%s: out = %v calls = %v", c.name, out, m.calls)
		}
		_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
		if out["state"] != "refused" || out["reason"] != reasonFastForward {
			t.Errorf("%s: state = %v", c.name, out)
		}
	}
}

// TestMergeChecksTheHeadBeforeTheRefusal asserts that a moved head is the
// reason, also when GitLab blocks the merge.
func TestMergeChecksTheHeadBeforeTheRefusal(t *testing.T) {
	_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: reply(200, openMR("success", "not_approved")),
	})
	_, _, out := call("merge_merge_request", map[string]any{"merge_request": ref, "sha": "cccccccccccccccccccccccccccccccccccccccc"})
	reason, _ := out["reason"].(string)
	if !strings.Contains(reason, "A push changed it") {
		t.Errorf("out = %v", out)
	}
}

// TestMergeRefusalReasonIsStable asserts that a refusal of the merge call
// gives the same reason on each call: the HTTP status and the message.
func TestMergeRefusalReasonIsStable(t *testing.T) {
	_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7:            reply(200, openMR("success", "mergeable")),
		"PUT " + api + mr7 + "/merge": reply(422, `{"message":"Branch cannot be merged"}`),
	})
	for range 2 {
		_, _, out := call("merge_merge_request", map[string]any{"merge_request": ref})
		if out["state"] != "refused" || out["reason"] != "GitLab refused the merge (HTTP 422): Branch cannot be merged" {
			t.Errorf("out = %v", out)
		}
	}
}
