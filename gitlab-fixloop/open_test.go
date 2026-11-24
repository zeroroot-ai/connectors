package main

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the GitLab REST API v4:
// https://docs.gitlab.com/api/repository_files/ (HEAD answers 200 or 404),
// https://docs.gitlab.com/api/commits/
// (201, and 400 "A branch called 'x' already exists." for a branch that exists) and
// https://docs.gitlab.com/api/merge_requests/ (create answers 201).
const srcBranch = "zeroroot/fix-sqli-1"

type routeMap = map[string]func(http.ResponseWriter, *http.Request, map[string]any)

func newMR() string {
	return strings.Replace(openMR("", "mergeable"), `"source_branch":"fix"`, `"source_branch":"`+srcBranch+`"`, 1)
}

func openRoutes() routeMap {
	return routeMap{
		"GET " + api + "/merge_requests":                reply(200, `[]`),
		"HEAD " + api + "/repository/files/src%2Fdb.go": reply(200, ``),
		"POST " + api + "/repository/commits":           reply(201, `{"id":"`+headSHA+`"}`),
		"POST " + api + "/merge_requests":               reply(201, newMR()),
	}
}

func openArgs() map[string]any {
	return map[string]any{
		"project": "acme/shop", "branch": srcBranch, "base_branch": "main",
		"title": "Fix SQL injection", "description": "Uses a bound parameter.",
		"files": []map[string]any{
			{"path": "src/db.go", "content": "package db\n"},
			{"path": "src/new.go", "content": "package db\n"},
			{"path": "old/unused.go", "delete": true},
		},
	}
}

func TestOpenMergeRequest(t *testing.T) {
	m, call := setup(t, openRoutes())
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr {
		t.Fatal(text)
	}
	if out["merge_request"] != ref || out["url"] != "https://gitlab.example.com/acme/shop/-/merge_requests/7" ||
		out["source_branch"] != srcBranch || out["created_at"] != "2026-11-20T07:30:00Z" || out["head_sha"] != headSHA || out["existing"] != nil {
		t.Errorf("out = %v", out)
	}
	checkOpenWrites(t, m)
}

const branchKey = "GET " + api + "/repository/branches/zeroroot%2Ffix-sqli-1"

func diffJS(newPath string, newFile, renamed, deleted bool) string {
	return diffModeJS(newPath, newFile, renamed, deleted, "", "")
}

// diffModeJS is a diff with the modes of the file. An empty mode is the usual
// one: 0 for the missing side of a new or a deleted file, and 100644 else.
func diffModeJS(newPath string, newFile, renamed, deleted bool, aMode, bMode string) string {
	if aMode == "" {
		aMode = "100644"
		if newFile {
			aMode = "0"
		}
	}
	if bMode == "" {
		bMode = "100644"
		if deleted {
			bMode = "0"
		}
	}
	return `{"old_path":"` + newPath + `","new_path":"` + newPath + `","a_mode":"` + aMode + `","b_mode":"` + bMode + `","new_file":` + strconv.FormatBool(newFile) +
		`,"renamed_file":` + strconv.FormatBool(renamed) + `,"deleted_file":` + strconv.FormatBool(deleted) + `,"diff":"@@"}`
}

// compareJS is the answer of GET /repository/compare: the commits of the
// branch that the base does not have, and the diffs.
func compareJS(commits int, diffs ...string) string {
	cs := make([]string, 0, commits)
	for range commits {
		cs = append(cs, `{"id":"`+headSHA+`"}`)
	}
	return `{"commits":[` + strings.Join(cs, ",") + `],"diffs":[` + strings.Join(diffs, ",") + `],"compare_timeout":false}`
}

// blobRoute answers a file only at the commit of the branch. A read at the
// name of the branch, or at another ref, gets 404, so a test fails when the
// tool reads there.
func blobRoute(content string) func(http.ResponseWriter, *http.Request, map[string]any) {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if got := r.URL.Query().Get("ref"); got != headSHA {
			http.Error(w, `{"message":"404 File Not Found (ref `+got+`)"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"file_name":"x","encoding":"base64","blob_id":"` + fixloop.BlobSHA(content) + `","content":""}`))
	}
}

// ownBranch answers the calls for the branch that openArgs() makes.
func ownBranch(routes routeMap, diffs ...string) routeMap {
	if len(diffs) == 0 {
		diffs = []string{diffJS("src/db.go", false, false, false), diffJS("src/new.go", true, false, false), diffJS("old/unused.go", false, false, true)}
	}
	routes[branchKey] = reply(200, `{"name":"`+srcBranch+`","commit":{"id":"`+headSHA+`"}}`)
	routes["GET "+api+"/repository/compare"] = reply(200, compareJS(1, diffs...))
	routes["GET "+api+"/repository/files/src%2Fdb.go"] = blobRoute("package db\n")
	routes["GET "+api+"/repository/files/src%2Fnew.go"] = blobRoute("package db\n")
	return routes
}

func TestOpenReturnsTheOpenMergeRequestOfAnOwnBranch(t *testing.T) {
	routes := ownBranch(openRoutes())
	routes["GET "+api+"/merge_requests"] = reply(200, `[`+newMR()+`]`)
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != true || out["merge_request"] != ref || out["head_sha"] != headSHA {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a repeat call wrote: %s", c)
		}
	}
}

// An own branch with no merge request is the result of a call that failed
// after the commit. The tool opens the merge request and makes no commit.
func TestOpenResumesAnOwnBranchWithNoMergeRequest(t *testing.T) {
	m, call := setup(t, ownBranch(openRoutes()))
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["merge_request"] != ref || out["existing"] != nil {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	if m.called("POST "+api+"/repository/commits") != 0 || m.called("POST "+api+"/merge_requests") != 1 {
		t.Errorf("calls = %v", m.calls)
	}
}

// TestOpenRefusesABranchWithAForeignCommit makes sure that the tool never
// builds on a branch that holds a commit that it did not make.
func TestOpenRefusesABranchWithAForeignCommit(t *testing.T) {
	good := []string{diffJS("src/db.go", false, false, false), diffJS("src/new.go", true, false, false), diffJS("old/unused.go", false, false, true)}
	cases := map[string]func(routeMap){
		"two commits": func(r routeMap) { r["GET "+api+"/repository/compare"] = reply(200, compareJS(2, good...)) },
		"no commit":   func(r routeMap) { r["GET "+api+"/repository/compare"] = reply(200, compareJS(0)) },
		"another file": func(r routeMap) {
			r["GET "+api+"/repository/compare"] = reply(200, compareJS(1, good[0], good[1], diffJS("evil.sh", true, false, false)))
		},
		"fewer files": func(r routeMap) { r["GET "+api+"/repository/compare"] = reply(200, compareJS(1, good[0], good[1])) },
		"not deleted": func(r routeMap) {
			r["GET "+api+"/repository/compare"] = reply(200, compareJS(1, good[0], good[1], diffJS("old/unused.go", false, false, false)))
		},
		"renamed": func(r routeMap) {
			r["GET "+api+"/repository/compare"] = reply(200, compareJS(1, diffJS("src/db.go", false, true, false), good[1], good[2]))
		},
		"another content": func(r routeMap) { r["GET "+api+"/repository/files/src%2Fnew.go"] = blobRoute("package evil\n") },
	}
	for name, mutate := range cases {
		routes := ownBranch(openRoutes())
		routes["GET "+api+"/merge_requests"] = reply(200, `[`+newMR()+`]`)
		mutate(routes)
		m, call := setup(t, routes)
		isErr, text, _ := call("open_merge_request", openArgs())
		if !isErr || !strings.Contains(text, "does not build on it") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "POST") || strings.HasPrefix(c, "PUT") || strings.HasPrefix(c, "DELETE") {
				t.Errorf("%s: the tool wrote: %s", name, c)
			}
		}
	}
}

func TestOpenShowsAGitLabError(t *testing.T) {
	routes := openRoutes()
	routes["POST "+api+"/merge_requests"] = reply(http.StatusConflict, `{"message":["Another open merge request already exists"]}`)
	_, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "Another open merge request") ||
		!strings.Contains(text, "the branch "+srcBranch+" stays") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	routes = openRoutes()
	routes["HEAD "+api+"/repository/files/src%2Fdb.go"] = reply(http.StatusInternalServerError, `{"message":"boom"}`)
	m, call := setup(t, routes)
	if isErr, _, _ := call("open_merge_request", openArgs()); !isErr {
		t.Error("a failed file check was ignored")
	}
	if m.called("POST "+api+"/repository/commits") != 0 {
		t.Error("the tool wrote after a failed file check")
	}
}

func TestOpenRefusesBadInput(t *testing.T) {
	m, call := setup(t, openRoutes())
	mutate := map[string]func(map[string]any){
		"project query":         func(a map[string]any) { a["project"] = "acme/shop?x=1" },
		"project dot dot":       func(a map[string]any) { a["project"] = "acme/../x" },
		"project empty segment": func(a map[string]any) { a["project"] = "acme//shop" },
		"same branch":           func(a map[string]any) { a["branch"] = "main" },
		"bad branch":            func(a map[string]any) { a["base_branch"] = "a b" },
		"no changes":            func(a map[string]any) { a["files"] = []map[string]any{} },
		"path escape":           func(a map[string]any) { a["files"] = []map[string]any{{"path": "a/../../x", "content": "a"}} },
		"git folder":            func(a map[string]any) { a["files"] = []map[string]any{{"path": ".git/config", "content": "a"}} },
		"content and delete":    func(a map[string]any) { a["files"] = []map[string]any{{"path": "a", "content": "a", "delete": true}} },
		"no title":              func(a map[string]any) { a["title"] = "" },
	}
	for name, f := range mutate {
		a := openArgs()
		f(a)
		if isErr, _, _ := call("open_merge_request", a); !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached GitLab: %v", m.calls)
	}
}

func TestOpenAnnotations(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITLAB_BASE_URL": "https://gitlab.example.com", "GITLAB_TOKEN": "t", "GITLAB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tl := kittest.Tools(t, kittest.Connect(t, register))["open_merge_request"]
	if tl == nil || tl.Annotations == nil || tl.Annotations.ReadOnlyHint || tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint {
		t.Fatalf("annotations = %+v", tl)
	}
	if !strings.Contains(tl.Description, "Undo: call close_merge_request") {
		t.Errorf("description does not say how a person undoes it: %s", tl.Description)
	}
}

func checkOpenWrites(t *testing.T, m *mock) {
	t.Helper()
	commit := m.bodies["POST "+api+"/repository/commits"]
	if commit["branch"] != srcBranch || commit["start_branch"] != "main" || commit["commit_message"] != "Fix SQL injection" {
		t.Errorf("commit = %v", commit)
	}
	acts, _ := commit["actions"].([]any)
	if len(acts) != 3 {
		t.Fatalf("actions = %v", acts)
	}
	want := []struct{ action, path string }{{"update", "src/db.go"}, {"create", "src/new.go"}, {"delete", "old/unused.go"}}
	for i, w := range want {
		a := acts[i].(map[string]any)
		if a["action"] != w.action || a["file_path"] != w.path {
			t.Errorf("action %d = %v, want %v", i, a, w)
		}
	}
	if acts[2].(map[string]any)["content"] != nil {
		t.Error("a delete carries content")
	}
	mrBody := m.bodies["POST "+api+"/merge_requests"]
	if mrBody["source_branch"] != srcBranch || mrBody["target_branch"] != "main" || mrBody["title"] != "Fix SQL injection" ||
		mrBody["description"] != "Uses a bound parameter." {
		t.Errorf("merge request = %v", mrBody)
	}
	if m.token != "glpat-secret" {
		t.Errorf("token = %q", m.token)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "PUT") || strings.HasPrefix(c, "DELETE") || strings.Contains(c, "/merge") && !strings.HasSuffix(c, "/merge_requests") {
			t.Errorf("unexpected call %s", c)
		}
	}
}

func TestCloseMergeRequest(t *testing.T) {
	m, call := setup(t, routeMap{
		"GET " + api + mr7: reply(200, openMR("success", "mergeable")),
		"PUT " + api + mr7: reply(200, openMR("success", "mergeable")),
	})
	isErr, text, out := call("close_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["closed"] != true || out["state"] != "closed" || out["merge_request"] != ref {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	if body := m.bodies["PUT "+api+mr7]; body["state_event"] != "close" || len(body) != 1 {
		t.Errorf("body = %v", body)
	}
}

func TestCloseMergeRequestPostsTheCommentFirst(t *testing.T) {
	m, call := setup(t, routeMap{
		"GET " + api + mr7:             reply(200, openMR("success", "mergeable")),
		"GET " + api + mr7 + "/notes":  reply(200, `[{"body":"another note"}]`),
		"POST " + api + mr7 + "/notes": reply(201, `{"id":1}`),
		"PUT " + api + mr7:             reply(200, openMR("success", "mergeable")),
	})
	isErr, text, out := call("close_merge_request", map[string]any{"merge_request": ref, "comment": "job j1 failed"})
	if isErr || out["closed"] != true {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	if body := m.bodies["POST "+api+mr7+"/notes"]; body["body"] != "job j1 failed" || len(body) != 1 {
		t.Errorf("note body = %v", body)
	}
	if got := m.calls; len(got) != 4 || !strings.HasPrefix(got[1], "GET") || !strings.HasPrefix(got[2], "POST") ||
		!strings.HasPrefix(got[3], "PUT") {
		t.Errorf("calls = %v, want the comment before the close", got)
	}
}

// A comment with the same body is posted once. A repeat of the call, such as
// the retry after a lost answer, finds the note and posts nothing.
func TestCloseMergeRequestPostsTheCommentOnceAfterALostAnswer(t *testing.T) {
	var stored []string
	m, call := setup(t, routeMap{
		"GET " + api + mr7: reply(200, openMR("success", "mergeable")),
		"GET " + api + mr7 + "/notes": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			items := make([]string, 0, len(stored))
			for _, b := range stored {
				items = append(items, `{"body":`+strconv.Quote(b)+`}`)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		},
		"POST " + api + mr7 + "/notes": func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			stored = append(stored, body["body"].(string)) // the write ran, and the answer is lost
			dropped(w, r, body)
		},
		"PUT " + api + mr7: reply(200, openMR("success", "mergeable")),
	})
	args := map[string]any{"merge_request": ref, "comment": "Fix job job-1 failed."}
	if isErr, _, _ := call("close_merge_request", args); !isErr {
		t.Fatal("the lost answer was not an error")
	}
	isErr, text, out := call("close_merge_request", args)
	if isErr || out["closed"] != true {
		t.Fatalf("retry: isErr=%v %s %v", isErr, text, out)
	}
	if n := m.called("POST " + api + mr7 + "/notes"); n != 1 {
		t.Errorf("the comment was posted %d times, want once", n)
	}
}

func TestCloseMergeRequestPostsNoCommentOnAClosedOne(t *testing.T) {
	closedMR := strings.Replace(openMR("success", "mergeable"), `"state":"opened"`, `"state":"closed"`, 1)
	m, call := setup(t, routeMap{"GET " + api + mr7: reply(200, closedMR)})
	isErr, _, out := call("close_merge_request", map[string]any{"merge_request": ref, "comment": "job j1 failed"})
	if isErr || out["closed"] != false || out["state"] != "closed" {
		t.Errorf("closed already: isErr=%v %v", isErr, out)
	}
	if len(m.calls) != 1 {
		t.Errorf("calls = %v, want one read", m.calls)
	}
}

func TestCloseMergeRequestRefusesALongComment(t *testing.T) {
	m, call := setup(t, nil)
	isErr, _, _ := call("close_merge_request", map[string]any{"merge_request": ref, "comment": strings.Repeat("x", fixloop.MaxCommentLen+1)})
	if !isErr || len(m.calls) != 0 {
		t.Errorf("isErr=%v calls=%v", isErr, m.calls)
	}
}

func TestCloseIsNotAnErrorWhenClosedAndRefusesMerged(t *testing.T) {
	closedMR := strings.Replace(openMR("success", "mergeable"), `"state":"opened"`, `"state":"closed"`, 1)
	m, call := setup(t, routeMap{"GET " + api + mr7: reply(200, closedMR)})
	isErr, _, out := call("close_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["closed"] != false || out["state"] != "closed" {
		t.Errorf("closed already: isErr=%v %v", isErr, out)
	}
	if m.called("PUT "+api+mr7) != 0 {
		t.Error("the tool wrote to a closed merge request")
	}
	m, call = setup(t, routeMap{"GET " + api + mr7: reply(200, mergedMR)})
	if isErr, text, _ := call("close_merge_request", map[string]any{"merge_request": ref}); !isErr || !strings.Contains(text, "merged") {
		t.Errorf("merged: isErr=%v %s", isErr, text)
	}
	if m.called("PUT "+api+mr7) != 0 {
		t.Error("the tool wrote to a merged merge request")
	}
	m, call = setup(t, nil)
	for _, bad := range []string{"", "acme/shop", "https://evil.example/acme/shop/-/merge_requests/7", "acme/shop!x"} {
		if isErr, _, _ := call("close_merge_request", map[string]any{"merge_request": bad}); !isErr {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("reached GitLab: %v", m.calls)
	}
}

func TestCloseAnnotations(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITLAB_BASE_URL": "https://gitlab.example.com", "GITLAB_TOKEN": "t", "GITLAB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tools := kittest.Tools(t, kittest.Connect(t, register))
	kittest.AssertReversible(t, tools, "open_merge_request", "close_merge_request")
	if a := tools["close_merge_request"].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("close annotations = %+v", a)
	}
}

// TestMergeRefusesAMovedHead makes sure that the sha from open_merge_request
// pins the merge: when a push moved the head, nothing is merged and the reason
// names both shas. When the head is the same, GitLab gets that sha.
func TestMergeRefusesAMovedHead(t *testing.T) {
	const moved = "cccccccccccccccccccccccccccccccccccccccc"
	movedMR := strings.Replace(openMR("success", "mergeable"), `"sha":"`+headSHA+`","merge_commit_sha"`, `"sha":"`+moved+`","merge_commit_sha"`, 1)
	m, call := setup(t, routeMap{"GET " + api + mr7: reply(200, movedMR)})
	isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": headSHA})
	reason, _ := out["reason"].(string)
	if isErr || out["merged"] != false || !strings.Contains(reason, "expected_head_sha is "+headSHA+", the head is "+moved) {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("PUT "+api+mr7+"/merge") != 0 {
		t.Error("the tool merged a moved head")
	}
	m, call = setup(t, routeMap{
		"GET " + api + mr7:            reply(200, openMR("success", "mergeable")),
		"PUT " + api + mr7 + "/merge": reply(200, mergedMR),
	})
	call("merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": headSHA})
	if got := m.bodies["PUT "+api+mr7+"/merge"]["sha"]; got != headSHA {
		t.Errorf("GitLab got sha %v", got)
	}
}

// TestOpenHeadIsTheCommitOfTheTool makes sure that head_sha is the commit that
// the tool made or checked, and that a merge request with another head is refused.
func TestOpenHeadIsTheCommitOfTheTool(t *testing.T) {
	const moved = "cccccccccccccccccccccccccccccccccccccccc"
	movedMR := strings.Replace(newMR(), `"sha":"`+headSHA+`","merge_commit_sha"`, `"sha":"`+moved+`","merge_commit_sha"`, 1)
	// Create path: GitLab shows another head than the new commit.
	routes := openRoutes()
	routes["POST "+api+"/merge_requests"] = reply(201, movedMR)
	_, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "A push moved the branch") {
		t.Errorf("create: isErr=%v %s", isErr, text)
	}
	// Resume path, open merge request with another head.
	routes = ownBranch(openRoutes())
	routes["GET "+api+"/merge_requests"] = reply(200, `[`+movedMR+`]`)
	_, call = setup(t, routes)
	if isErr, _, _ := call("open_merge_request", openArgs()); !isErr {
		t.Error("resume: the head was not checked")
	}
	// Resume path, a new merge request with another head.
	routes = ownBranch(openRoutes())
	routes["POST "+api+"/merge_requests"] = reply(201, movedMR)
	_, call = setup(t, routes)
	if isErr, _, _ := call("open_merge_request", openArgs()); !isErr {
		t.Error("resume with a new merge request: the head was not checked")
	}
	// The branch head is not the commit of the compare.
	routes = ownBranch(openRoutes())
	routes[branchKey] = reply(200, `{"name":"`+srcBranch+`","commit":{"id":"`+moved+`"}}`)
	m, call := setup(t, routes)
	isErr, text, _ := call("open_merge_request", openArgs())
	if !isErr || !strings.Contains(text, "head that is not the commit that this tool checked") {
		t.Errorf("moved branch: isErr=%v %s", isErr, text)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("the tool wrote: %s", c)
		}
	}
	// A commit with no id is an error.
	routes = openRoutes()
	routes["POST "+api+"/repository/commits"] = reply(201, `{}`)
	_, call = setup(t, routes)
	if isErr, _, _ := call("open_merge_request", openArgs()); !isErr {
		t.Error("a commit with no id was accepted")
	}
}

// A merge request from a fork with the same branch name is not ours.
func TestOpenIgnoresAMergeRequestFromAFork(t *testing.T) {
	fork := strings.Replace(newMR(), `"source_project_id":3`, `"source_project_id":9`, 1)
	routes := ownBranch(openRoutes())
	routes["GET "+api+"/merge_requests"] = reply(200, `[`+fork+`]`)
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != nil || m.called("POST "+api+"/merge_requests") != 1 {
		t.Errorf("isErr=%v %s %v calls %v", isErr, text, out, m.calls)
	}
}

// The tool refuses a branch whose commit changes a file mode that it did not change.
func TestOpenRefusesAChangeOfMode(t *testing.T) {
	for name, diffs := range map[string][]string{
		"new file with mode 100755": {diffJS("src/db.go", false, false, false), diffModeJS("src/new.go", true, false, false, "", "100755"), diffJS("old/unused.go", false, false, true)},
		"mode change of a file":     {diffModeJS("src/db.go", false, false, false, "100755", "100644"), diffJS("src/new.go", true, false, false), diffJS("old/unused.go", false, false, true)},
	} {
		routes := ownBranch(openRoutes(), diffs...)
		m, call := setup(t, routes)
		isErr, text, _ := call("open_merge_request", openArgs())
		if !isErr || !strings.Contains(text, "file mode") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "POST") {
				t.Errorf("%s: the tool wrote: %s", name, c)
			}
		}
	}
}
