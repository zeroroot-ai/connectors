package main

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// The mock follows the git data API and the pull request API of GitHub:
// https://docs.github.com/en/rest/git/blobs#create-a-blob,
// https://docs.github.com/en/rest/git/trees#create-a-tree,
// https://docs.github.com/en/rest/git/commits#create-a-commit,
// https://docs.github.com/en/rest/git/refs#create-a-reference and
// https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request.
// A reference that does not exist answers 404 (the default of the mock).
const (
	newBlobSHA = "1111111111111111111111111111111111111111"
	newTreeSHA = "2222222222222222222222222222222222222222"
	newComSHA  = "3333333333333333333333333333333333333333"
	baseTree   = "4444444444444444444444444444444444444444"
	srcBranch  = "zeroroot/fix-sqli-1"
)

// openRoutes answers the reads and the writes of one open_merge_request call.
func openRoutes() map[string]handler {
	return map[string]handler{
		"GET " + repo + "/git/ref/heads/main":     reply(200, `{"ref":"refs/heads/main","object":{"sha":"`+baseSHA+`"}}`),
		"GET " + repo + "/git/commits/" + baseSHA: reply(200, `{"sha":"`+baseSHA+`","tree":{"sha":"`+baseTree+`"},"parents":[]}`),
		"POST " + repo + "/git/blobs":             reply(201, `{"sha":"`+newBlobSHA+`"}`),
		"POST " + repo + "/git/trees":             reply(201, `{"sha":"`+newTreeSHA+`"}`),
		"POST " + repo + "/git/commits":           reply(201, `{"sha":"`+newComSHA+`","tree":{"sha":"`+newTreeSHA+`"},"parents":[{"sha":"`+baseSHA+`"}]}`),
		"POST " + repo + "/git/refs":              reply(201, `{"ref":"refs/heads/`+srcBranch+`"}`),
		"POST " + repo + "/pulls":                 reply(201, strings.Replace(openPR("clean"), `"ref":"fix"`, `"ref":"`+srcBranch+`"`, 1)),
	}
}

func openArgs() map[string]any {
	return map[string]any{
		"project": "acme/shop", "branch": srcBranch, "base_branch": "main",
		"title": "Fix SQL injection", "description": "Uses a bound parameter.",
		"files": []map[string]any{
			{"path": "src/db.go", "content": "package db\n"},
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
	if out["merge_request"] != ref || out["url"] != "https://github.com/acme/shop/pull/7" || out["source_branch"] != srcBranch ||
		out["created_at"] != "2026-11-20T06:30:00Z" || out["existing"] != nil {
		t.Errorf("out = %v", out)
	}
	checkOpenWrites(t, m)
}

func TestOpenReturnsTheOpenPullRequestOfAnExistingBranch(t *testing.T) {
	routes := openRoutes()
	routes["GET "+repo+"/git/ref/heads/"+srcBranch] = reply(200, `{"object":{"sha":"`+newComSHA+`"}}`)
	routes["GET "+repo+"/pulls"] = reply(200, `[`+strings.Replace(openPR("clean"), `"ref":"fix"`, `"ref":"`+srcBranch+`"`, 1)+`]`)
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

func TestOpenRefusesAnExistingBranchWithNoPullRequest(t *testing.T) {
	routes := openRoutes()
	routes["GET "+repo+"/git/ref/heads/"+srcBranch] = reply(200, `{"object":{"sha":"`+newComSHA+`"}}`)
	routes["GET "+repo+"/pulls"] = reply(200, `[]`)
	m, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "Use another branch name") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("wrote: %s", c)
		}
	}
}

func TestOpenShowsAGitHubError(t *testing.T) {
	routes := openRoutes()
	routes["POST "+repo+"/pulls"] = reply(http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)
	_, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "Validation Failed") ||
		!strings.Contains(text, "the branch "+srcBranch+" stays") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
	routes = openRoutes()
	delete(routes, "GET "+repo+"/git/ref/heads/main")
	_, call = setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "read the target branch") {
		t.Errorf("missing target: isErr=%v %s", isErr, text)
	}
}

func TestOpenRefusesBadInput(t *testing.T) {
	m, call := setup(t, openRoutes())
	mutate := map[string]func(map[string]any){
		"project without owner": func(a map[string]any) { a["project"] = "shop" },
		"project path":          func(a map[string]any) { a["project"] = "acme/shop/../x" },
		"project dot dot":       func(a map[string]any) { a["project"] = "acme/.." },
		"same branch":           func(a map[string]any) { a["branch"] = "main" },
		"bad branch":            func(a map[string]any) { a["branch"] = "a..b" },
		"no changes":            func(a map[string]any) { a["files"] = []map[string]any{} },
		"path escape":           func(a map[string]any) { a["files"] = []map[string]any{{"path": "../x", "content": "a"}} },
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
		t.Errorf("a bad input reached GitHub: %v", m.calls)
	}
}

func TestOpenAnnotations(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{"GITHUB_TOKEN": "t", "GITHUB_ENVIRONMENT": "production"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tools := kittest.Tools(t, kittest.Connect(t, register))
	tl := tools["open_merge_request"]
	a := tl.Annotations
	if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Fatalf("annotations = %+v", a)
	}
	if !strings.Contains(tl.Description, "Undo: call close_merge_request") {
		t.Errorf("description does not say how a person undoes it: %s", tl.Description)
	}
}

func checkOpenWrites(t *testing.T, m *mock) {
	t.Helper()
	blob := m.bodies["POST "+repo+"/git/blobs"]
	if blob["encoding"] != "base64" || blob["content"] != base64.StdEncoding.EncodeToString([]byte("package db\n")) {
		t.Errorf("blob = %v", blob)
	}
	tree := m.bodies["POST "+repo+"/git/trees"]
	entries, _ := tree["tree"].([]any)
	if tree["base_tree"] != baseTree || len(entries) != 2 {
		t.Fatalf("tree = %v", tree)
	}
	if e := entries[0].(map[string]any); e["path"] != "src/db.go" || e["sha"] != newBlobSHA || e["mode"] != "100644" {
		t.Errorf("entry 0 = %v", e)
	}
	// A null sha removes the path.
	if e := entries[1].(map[string]any); e["path"] != "old/unused.go" || e["sha"] != nil {
		t.Errorf("entry 1 = %v", e)
	}
	commit := m.bodies["POST "+repo+"/git/commits"]
	if commit["tree"] != newTreeSHA || commit["message"] != "Fix SQL injection" || commit["parents"].([]any)[0] != baseSHA {
		t.Errorf("commit = %v", commit)
	}
	if refs := m.bodies["POST "+repo+"/git/refs"]; refs["ref"] != "refs/heads/"+srcBranch || refs["sha"] != newComSHA {
		t.Errorf("refs = %v", refs)
	}
	pr := m.bodies["POST "+repo+"/pulls"]
	if pr["head"] != srcBranch || pr["base"] != "main" || pr["title"] != "Fix SQL injection" || pr["body"] != "Uses a bound parameter." {
		t.Errorf("pull = %v", pr)
	}
	// The tool never writes to the target branch, and never merges.
	for _, c := range m.calls {
		if strings.HasPrefix(c, "PATCH") || strings.HasPrefix(c, "PUT") || strings.HasPrefix(c, "DELETE") || strings.Contains(c, "/merge") {
			t.Errorf("unexpected call %s", c)
		}
	}
	if tok := m.headers.Get("Authorization"); tok != "Bearer ghp_secret" {
		t.Errorf("auth = %q", tok)
	}
}

func TestCloseMergeRequest(t *testing.T) {
	m, call := setup(t, map[string]handler{
		"GET " + pr7:   reply(200, openPR("clean")),
		"PATCH " + pr7: reply(200, openPR("clean")),
	})
	isErr, text, out := call("close_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["closed"] != true || out["state"] != "closed" || out["merge_request"] != ref {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	if body := m.bodies["PATCH "+pr7]; body["state"] != "closed" || len(body) != 1 {
		t.Errorf("body = %v", body)
	}
}

func TestCloseIsNotAnErrorWhenClosedAndRefusesMerged(t *testing.T) {
	m, call := setup(t, map[string]handler{"GET " + pr7: reply(200, pullJSON("closed", false, "null", "unknown", headSHA))})
	isErr, _, out := call("close_merge_request", map[string]any{"merge_request": ref})
	if isErr || out["closed"] != false || out["state"] != "closed" {
		t.Errorf("closed already: isErr=%v %v", isErr, out)
	}
	if m.called("PATCH "+pr7) != 0 {
		t.Error("the tool wrote to a closed pull request")
	}
	m, call = setup(t, map[string]handler{"GET " + pr7: reply(200, mergedPR)})
	if isErr, text, _ := call("close_merge_request", map[string]any{"merge_request": ref}); !isErr || !strings.Contains(text, "merged") {
		t.Errorf("merged: isErr=%v %s", isErr, text)
	}
	if m.called("PATCH "+pr7) != 0 {
		t.Error("the tool wrote to a merged pull request")
	}
	m, call = setup(t, nil)
	for _, bad := range []string{"", "acme/shop", "https://evil.example/acme/shop/pull/7", "acme/shop#x"} {
		if isErr, _, _ := call("close_merge_request", map[string]any{"merge_request": bad}); !isErr {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("reached GitHub: %v", m.calls)
	}
}

func TestCloseAnnotations(t *testing.T) {
	register, err := build(kit.NewEnvFrom(map[string]string{"GITHUB_TOKEN": "t", "GITHUB_ENVIRONMENT": "production"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tools := kittest.Tools(t, kittest.Connect(t, register))
	kittest.AssertReversible(t, tools, "open_merge_request", "close_merge_request")
	if a := tools["close_merge_request"].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("close annotations = %+v", a)
	}
}
