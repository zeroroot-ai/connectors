package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
// https://docs.gitlab.com/api/branches/, https://docs.gitlab.com/api/repositories/
// and https://docs.gitlab.com/api/deployments/.
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

func setup(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request, map[string]any)) (*mock, func(string, map[string]any) (bool, string, map[string]any)) {
	t.Helper()
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

func TestRefusalShowsInStateUntilTheHeadChanges(t *testing.T) {
	head := openMR("success", "not_approved")
	_, call := setup(t, map[string]func(http.ResponseWriter, *http.Request, map[string]any){
		"GET " + api + mr7: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(head)) },
	})
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "open" {
		t.Fatalf("before a try the state is open: %v", out)
	}
	call("merge_merge_request", map[string]any{"merge_request": ref})
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if out["state"] != "refused" || !strings.Contains(out["reason"].(string), "not_approved") {
		t.Errorf("after the refusal: %v", out)
	}
	head = strings.ReplaceAll(head, headSHA, "dddddddddddddddddddddddddddddddddddddddd")
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

func revertRoutes(found string, revertMR string) map[string]func(http.ResponseWriter, *http.Request, map[string]any) {
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

func TestRevertKeepsABranchThatHoldsTheRevert(t *testing.T) {
	routes := revertRoutes(`[]`, revertMRJSON)
	routes["POST "+api+"/repository/branches"] = reply(400, `{"message":"Branch already exists"}`)
	routes["GET "+api+"/repository/compare"] = reply(200, `{"commits":[{"id":"eeee"}]}`)
	m, call := setup(t, routes)
	isErr, text, out := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["merged"] != true {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("POST "+api+"/repository/commits/"+mergeSHA+"/revert") != 0 {
		t.Error("reverted a second time")
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
	_, call := setup(t, deployments(`[`+
		`{"id":40,"sha":"2222222222222222222222222222222222222222","status":"success","updated_at":"2026-11-20T09:01:00Z"},`+
		`{"id":42,"sha":"1111111111111111111111111111111111111111","status":"success","updated_at":"2026-11-20T09:10:00Z"}]`))
	_, _, out := call("deployment_state", map[string]any{"merge_request": ref})
	if out["deployed"] != true || out["deployment"] != "acme/shop/deployments/42" || out["at"] != "2026-11-20T09:10:00Z" {
		t.Errorf("out = %v", out)
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

func TestDeploymentQuery(t *testing.T) {
	var query string
	routes := deployments(`[]`)
	routes["GET "+api+"/deployments"] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}
	_, call := setup(t, routes)
	call("deployment_state", map[string]any{"merge_request": ref})
	for _, want := range []string{"environment=production", "status=success", "updated_after=2026-11-20T09%3A00%3A00Z"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %s", query, want)
		}
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
