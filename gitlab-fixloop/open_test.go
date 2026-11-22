package main

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

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

func TestOpenReturnsTheOpenMergeRequestOfAnExistingBranch(t *testing.T) {
	routes := openRoutes()
	routes["GET "+api+"/merge_requests"] = reply(200, `[`+newMR()+`]`)
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != true || out["merge_request"] != ref {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a repeat call wrote: %s", c)
		}
	}
}

func TestOpenRefusesAnExistingBranchWithNoMergeRequest(t *testing.T) {
	routes := openRoutes()
	routes["POST "+api+"/repository/commits"] = reply(http.StatusBadRequest, `{"message":"A branch called 'zeroroot/fix-sqli-1' already exists."}`)
	m, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "Use another branch name") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	if m.called("POST "+api+"/merge_requests") != 0 {
		t.Error("the tool opened a merge request")
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
	if isErr || out["merged"] != false || !strings.Contains(reason, "moved: expected "+headSHA+", now "+moved) {
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
