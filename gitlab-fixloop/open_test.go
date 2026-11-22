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
		"repo": "acme/shop", "source_branch": srcBranch, "target_branch": "main",
		"title": "Fix SQL injection", "body": "Uses a bound parameter.",
		"changes": []map[string]any{
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
		out["source_branch"] != srcBranch || out["created_at"] != "2026-11-20T07:30:00Z" || out["existing"] != nil {
		t.Errorf("out = %v", out)
	}
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
		"repo query":         func(a map[string]any) { a["repo"] = "acme/shop?x=1" },
		"repo dot dot":       func(a map[string]any) { a["repo"] = "acme/../x" },
		"repo empty segment": func(a map[string]any) { a["repo"] = "acme//shop" },
		"same branch":        func(a map[string]any) { a["source_branch"] = "main" },
		"bad branch":         func(a map[string]any) { a["target_branch"] = "a b" },
		"no changes":         func(a map[string]any) { a["changes"] = []map[string]any{} },
		"path escape":        func(a map[string]any) { a["changes"] = []map[string]any{{"path": "a/../../x", "content": "a"}} },
		"git folder":         func(a map[string]any) { a["changes"] = []map[string]any{{"path": ".git/config", "content": "a"}} },
		"content and delete": func(a map[string]any) { a["changes"] = []map[string]any{{"path": "a", "content": "a", "delete": true}} },
		"no title":           func(a map[string]any) { a["title"] = "" },
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
	if !strings.Contains(tl.Description, "closing the merge request") {
		t.Errorf("description does not say how a person undoes it: %s", tl.Description)
	}
}
