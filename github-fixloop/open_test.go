package main

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
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
	srcTree    = "5555555555555555555555555555555555555555"
	oldTree    = "6666666666666666666666666666666666666666"
	srcBranch  = "zeroroot/fix-sqli-1"
)

// openRoutes answers the reads and the writes of one open_merge_request call.
func openRoutes() map[string]handler {
	return map[string]handler{
		"GET " + repo + "/git/ref/heads/main":     reply(200, `{"ref":"refs/heads/main","object":{"sha":"`+baseSHA+`"}}`),
		"GET " + repo + "/git/commits/" + baseSHA: reply(200, `{"sha":"`+baseSHA+`","tree":{"sha":"`+baseTree+`"},"parents":[]}`),
		"GET " + repo + "/git/trees/" + baseTree:  reply(200, `{"sha":"`+baseTree+`","truncated":false,"tree":[{"path":"old","mode":"040000","type":"tree","sha":"`+oldTree+`"},{"path":"src","mode":"040000","type":"tree","sha":"`+srcTree+`"}]}`),
		"GET " + repo + "/git/trees/" + oldTree:   reply(200, `{"sha":"`+oldTree+`","truncated":false,"tree":[{"path":"unused.go","mode":"100644","type":"blob","sha":"`+blobSHA+`"}]}`),
		"GET " + repo + "/git/trees/" + srcTree:   reply(200, `{"sha":"`+srcTree+`","truncated":false,"tree":[{"path":"db.go","mode":"100644","type":"blob","sha":"`+blobSHA+`"}]}`),
		"POST " + repo + "/git/blobs":             reply(201, `{"sha":"`+newBlobSHA+`"}`),
		"POST " + repo + "/git/trees":             reply(201, `{"sha":"`+newTreeSHA+`"}`),
		"POST " + repo + "/git/commits":           reply(201, `{"sha":"`+newComSHA+`","tree":{"sha":"`+newTreeSHA+`"},"parents":[{"sha":"`+baseSHA+`"}]}`),
		"POST " + repo + "/git/refs":              reply(201, `{"ref":"refs/heads/`+srcBranch+`"}`),
		"POST " + repo + "/pulls":                 reply(201, ownPR(newComSHA)),
	}
}

// ownPR is a pull request from the source branch whose head is head.
func ownPR(head string) string {
	return strings.Replace(pullJSON("open", false, "true", "clean", head), `"ref":"fix"`, `"ref":"`+srcBranch+`"`, 1)
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
		out["created_at"] != "2026-11-20T06:30:00Z" || out["head_sha"] != newComSHA || out["existing"] != nil {
		t.Errorf("out = %v", out)
	}
	checkOpenWrites(t, m)
}

// compareJS is the answer of GET /compare/{base}...{head}: commits ahead of
// the base, and the files with their status and blob sha.
func compareJS(ahead int, files ...string) string {
	commits := make([]string, 0, ahead)
	for range ahead {
		commits = append(commits, `{"sha":"`+newComSHA+`"}`)
	}
	return `{"ahead_by":` + itoa(ahead) + `,"commits":[` + strings.Join(commits, ",") + `],"files":[` + strings.Join(files, ",") + `]}`
}

func cmpFile(name, status, content string) string {
	return `{"filename":"` + name + `","status":"` + status + `","sha":"` + fixloop.BlobSHA(content) + `"}`
}

const compareKey = "GET " + repo + "/compare/main...zeroroot%2Ffix-sqli-1"

// ownCompare is the compare answer of the branch that openArgs() makes.
func ownCompare() string {
	return compareJS(1, cmpFile("src/db.go", "modified", "package db\n"), `{"filename":"old/unused.go","status":"removed","sha":"`+blobSHA+`"}`)
}

func branchExists(routes map[string]handler) map[string]handler {
	routes["GET "+repo+"/git/ref/heads/"+srcBranch] = reply(200, `{"object":{"sha":"`+newComSHA+`"}}`)
	return ownTree(routes, `{"path":"src/db.go","mode":"100644","type":"blob","sha":"`+fixloop.BlobSHA("package db\n")+`"}`)
}

// ownTree answers the commit of the branch and its recursive tree, which the
// resume reads to check the file mode. The entries are the files that exist.
func ownTree(routes map[string]handler, entries ...string) map[string]handler {
	routes["GET "+repo+"/git/commits/"+newComSHA] = reply(200, `{"sha":"`+newComSHA+`","tree":{"sha":"`+newTreeSHA+`"},"parents":[{"sha":"`+baseSHA+`"}]}`)
	routes["GET "+repo+"/git/trees/"+newTreeSHA] = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Query().Get("recursive") != "1" {
			http.Error(w, `{"message":"not recursive"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"sha":"` + newTreeSHA + `","truncated":false,"tree":[` + strings.Join(entries, ",") + `]}`))
	}
	return routes
}

func TestOpenReturnsTheOpenPullRequestOfAnOwnBranch(t *testing.T) {
	routes := branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(newComSHA)+`]`)
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != true || out["merge_request"] != ref || out["head_sha"] != newComSHA {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a repeat call wrote: %s", c)
		}
	}
}

// An own branch with no pull request is the result of a call that failed after
// it made the branch. The tool opens the pull request and makes no commit.
func TestOpenResumesAnOwnBranchWithNoPullRequest(t *testing.T) {
	routes := branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[]`)
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["merge_request"] != ref || out["existing"] != nil {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") && c != "POST "+repo+"/pulls" {
			t.Errorf("the tool wrote on the branch: %s", c)
		}
	}
	if m.called("POST "+repo+"/pulls") != 1 {
		t.Error("no pull request was opened")
	}
}

// TestOpenRefusesABranchWithAForeignCommit makes sure that the tool never
// builds on a branch that holds a commit that it did not make.
func TestOpenRefusesABranchWithAForeignCommit(t *testing.T) {
	good := []string{cmpFile("src/db.go", "modified", "package db\n"), `{"filename":"old/unused.go","status":"removed","sha":"` + blobSHA + `"}`}
	for name, cmp := range map[string]string{
		"two commits":      compareJS(2, good...),
		"no commit":        compareJS(0),
		"another file":     compareJS(1, cmpFile("src/db.go", "modified", "package db\n"), cmpFile("evil.sh", "added", "x")),
		"fewer files":      compareJS(1, good[0]),
		"another content":  compareJS(1, cmpFile("src/db.go", "modified", "package evil\n"), good[1]),
		"file not removed": compareJS(1, good[0], cmpFile("old/unused.go", "modified", "x")),
		"renamed":          compareJS(1, cmpFile("src/db.go", "renamed", "package db\n"), good[1]),
		// GitHub "changed" is a change of the mode or the type, for example a symlink.
		// The blob matches, and the branch is still refused.
		"status changed with the same blob": compareJS(1, cmpFile("src/db.go", "changed", "package db\n"), good[1]),
	} {
		routes := branchExists(openRoutes())
		routes[compareKey] = reply(200, cmp)
		routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(newComSHA)+`]`)
		m, call := setup(t, routes)
		isErr, text, _ := call("open_merge_request", openArgs())
		if !isErr || !strings.Contains(text, "does not build on it") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "POST") || strings.HasPrefix(c, "PATCH") || strings.HasPrefix(c, "DELETE") {
				t.Errorf("%s: the tool wrote: %s", name, c)
			}
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
	checkTreeWrites(t, m)
	checkCommitWrites(t, m)
}

func checkTreeWrites(t *testing.T, m *mock) {
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
}

func checkCommitWrites(t *testing.T, m *mock) {
	t.Helper()
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

func TestCloseMergeRequestPostsTheCommentFirst(t *testing.T) {
	m, call := setup(t, map[string]handler{
		"GET " + pr7:                          reply(200, openPR("clean")),
		"GET " + repo + "/issues/7/comments":  reply(200, `[{"body":"another comment"}]`),
		"POST " + repo + "/issues/7/comments": reply(201, `{"id":1}`),
		"PATCH " + pr7:                        reply(200, openPR("clean")),
	})
	isErr, text, out := call("close_merge_request", map[string]any{"merge_request": ref, "comment": "job j1 failed"})
	if isErr || out["closed"] != true {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	if body := m.bodies["POST "+repo+"/issues/7/comments"]; body["body"] != "job j1 failed" || len(body) != 1 {
		t.Errorf("comment body = %v", body)
	}
	if got := m.calls; len(got) != 4 || !strings.HasPrefix(got[1], "GET") || !strings.HasPrefix(got[2], "POST") ||
		!strings.HasPrefix(got[3], "PATCH") {
		t.Errorf("calls = %v, want the comment before the close", got)
	}
}

// A comment with the same body is posted once. A repeat of the call, such as
// the retry after a lost answer, finds the comment and posts nothing.
func TestCloseMergeRequestPostsTheCommentOnceAfterALostAnswer(t *testing.T) {
	var stored []string
	m, call := setup(t, map[string]handler{
		"GET " + pr7: reply(200, openPR("clean")),
		"GET " + repo + "/issues/7/comments": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			items := make([]string, 0, len(stored))
			for _, b := range stored {
				items = append(items, `{"body":`+strconv.Quote(b)+`}`)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		},
		"POST " + repo + "/issues/7/comments": func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			stored = append(stored, body["body"].(string)) // the write ran, and the answer is lost
			dropped(w, r, body)
		},
		"PATCH " + pr7: reply(200, openPR("clean")),
	})
	args := map[string]any{"merge_request": ref, "comment": "Fix job job-1 failed."}
	if isErr, _, _ := call("close_merge_request", args); !isErr {
		t.Fatal("the lost answer was not an error")
	}
	isErr, text, out := call("close_merge_request", args)
	if isErr || out["closed"] != true {
		t.Fatalf("retry: isErr=%v %s %v", isErr, text, out)
	}
	if n := m.called("POST " + repo + "/issues/7/comments"); n != 1 {
		t.Errorf("the comment was posted %d times, want once", n)
	}
}

func TestCloseMergeRequestPostsNoCommentOnAClosedOne(t *testing.T) {
	m, call := setup(t, map[string]handler{"GET " + pr7: reply(200, pullJSON("closed", false, "null", "unknown", headSHA))})
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

// TestMergeRefusesAMovedHead makes sure that the sha from open_merge_request
// pins the merge: when a push moved the head, nothing is merged and the reason
// names both shas. When the head is the same, GitHub gets that sha.
func TestMergeRefusesAMovedHead(t *testing.T) {
	const moved = "dddddddddddddddddddddddddddddddddddddddd"
	m, call := setup(t, merge(repoRules(), map[string]handler{
		"GET " + pr7: reply(200, pullJSON("open", false, "true", "clean", moved)),
		"GET " + repo + "/commits/" + moved + "/check-runs": reply(200, `{"total_count":1,"check_runs":[`+greenRun+`]}`),
		"GET " + repo + "/commits/" + moved + "/status":     reply(200, combinedJSON("pending", 0, 0, 0)),
	}))
	isErr, text, out := call("merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": headSHA})
	reason, _ := out["reason"].(string)
	if isErr || out["merged"] != false || out["head_sha"] != moved || !strings.Contains(reason, "expected_head_sha is "+headSHA+", the head is "+moved) {
		t.Fatalf("isErr=%v out=%v %s", isErr, out, text)
	}
	if m.called("PUT "+pr7+"/merge") != 0 {
		t.Error("the tool merged a moved head")
	}
	m, call = setup(t, merge(checks(greenRun, "success", 1), map[string]handler{
		"GET " + pr7: reply(200, openPR("clean")), "PUT " + pr7 + "/merge": reply(200, `{"merged":true}`),
	}))
	call("merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": headSHA})
	if got := m.bodies["PUT "+pr7+"/merge"]["sha"]; got != headSHA {
		t.Errorf("GitHub got sha %v", got)
	}
}

// TestOpenHeadIsTheCommitOfTheTool makes sure that head_sha is the commit that
// the tool made or checked, and that a pull request with another head is refused.
func TestOpenHeadIsTheCommitOfTheTool(t *testing.T) {
	const moved = "dddddddddddddddddddddddddddddddddddddddd"
	// Create path: GitHub shows another head than the new commit.
	routes := openRoutes()
	routes["POST "+repo+"/pulls"] = reply(201, ownPR(moved))
	_, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "not "+newComSHA) {
		t.Errorf("create: isErr=%v %s", isErr, text)
	}
	// Resume path, open pull request with another head.
	routes = branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(moved)+`]`)
	_, call = setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "A push moved the branch") {
		t.Errorf("resume: isErr=%v %s", isErr, text)
	}
	// Resume path, a new pull request with another head.
	routes = branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[]`)
	routes["POST "+repo+"/pulls"] = reply(201, ownPR(moved))
	_, call = setup(t, routes)
	if isErr, _, _ := call("open_merge_request", openArgs()); !isErr {
		t.Error("resume with a new pull request: the head was not checked")
	}
	// The branch moved after the compare: its head is not the commit that the compare showed.
	routes = branchExists(openRoutes())
	routes["GET "+repo+"/git/ref/heads/"+srcBranch] = reply(200, `{"object":{"sha":"`+moved+`"}}`)
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(newComSHA)+`]`)
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
}

// A pull request from a fork with the same branch name is not ours.
func TestOpenIgnoresAPullRequestFromAFork(t *testing.T) {
	fork := strings.Replace(ownPR(newComSHA), `"head":{"sha":"`+newComSHA+`","ref":"`+srcBranch+`","repo":{"full_name":"acme/shop"}}`,
		`"head":{"sha":"`+newComSHA+`","ref":"`+srcBranch+`","repo":{"full_name":"evil/shop"}}`, 1)
	routes := branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[`+fork+`]`)
	routes["POST "+repo+"/pulls"] = reply(201, ownPR(newComSHA))
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != nil {
		t.Fatalf("isErr=%v %s %v", isErr, text, out)
	}
	if m.called("POST "+repo+"/pulls") != 1 {
		t.Error("the tool used the pull request of the fork")
	}
}

// The compare answer has no file mode, so the resume reads the tree of the
// commit. A mode other than 100644, a type other than blob, a cut tree and a
// path that should be removed are all refused, and nothing is written.
func TestOpenRefusesAForeignModeInTheTree(t *testing.T) {
	blob := fixloop.BlobSHA("package db\n")
	entry := func(mode, typ string) string {
		return `{"path":"src/db.go","mode":"` + mode + `","type":"` + typ + `","sha":"` + blob + `"}`
	}
	cases := map[string][]string{
		"executable":        {entry("100755", "blob")},
		"symlink":           {entry("120000", "blob")},
		"submodule":         {entry("160000", "commit")},
		"a tree not a blob": {entry("040000", "tree")},
		// Each case below passes every other check, so it fails only when the
		// check that it names is gone: the mode, the type or the blob sha.
		"type commit with mode 100644": {entry("100644", "commit")},
		"other blob with mode 100644":  {`{"path":"src/db.go","mode":"100644","type":"blob","sha":"` + fixloop.BlobSHA("package evil\n") + `"}`},
		"path missing":                 {},
		"deleted file kept":            {entry("100644", "blob"), `{"path":"old/unused.go","mode":"100644","type":"blob","sha":"` + blobSHA + `"}`},
	}
	for name, entries := range cases {
		routes := branchExists(openRoutes())
		ownTree(routes, entries...)
		routes[compareKey] = reply(200, ownCompare())
		routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(newComSHA)+`]`)
		m, call := setup(t, routes)
		isErr, text, _ := call("open_merge_request", openArgs())
		if !isErr || !strings.Contains(text, "does not build on it") {
			t.Errorf("%s: isErr=%v %s", name, isErr, text)
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "POST") {
				t.Errorf("%s: the tool wrote: %s", name, c)
			}
		}
	}
	routes := branchExists(openRoutes())
	routes["GET "+repo+"/git/trees/"+newTreeSHA] = reply(200, `{"truncated":true,"tree":[]}`)
	routes[compareKey] = reply(200, ownCompare())
	_, call := setup(t, routes)
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "too large") {
		t.Errorf("truncated tree: isErr=%v %s", isErr, text)
	}
}
