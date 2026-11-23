package main

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
	"github.com/zeroroot-ai/connectors/internal/mockforge/mocktest"
)

// These tests run the connector against the stateful mock forge
// (internal/mockforge). The mock keeps repositories, branches, commits and pull
// requests, so a test follows one change from the open to the revert.

const (
	flawed = "package db\n// flaw: SQL injection\n"
	fixed  = "package db\n// fixed: bound parameter\n"
)

func forgeSetup(t *testing.T) (*mocktest.Server, func(string, map[string]any) (bool, string, map[string]any)) {
	t.Helper()
	mergeableWait = 0
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{"src/db.go": flawed, "run.sh": "#!/bin/sh\n"},
		map[string]any{"auto_deploy": map[string]string{"environment": "production", "state": "success"}})
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

func forgeOpen(t *testing.T, call func(string, map[string]any) (bool, string, map[string]any)) map[string]any {
	t.Helper()
	isErr, text, out := call("open_merge_request", map[string]any{
		"project": "acme/shop", "branch": "zeroroot/fix-1", "base_branch": "main",
		"title": "Fix SQL injection", "description": "Uses a bound parameter.",
		"files": []map[string]any{{"path": "src/db.go", "content": fixed}},
	})
	if isErr {
		t.Fatalf("open: %s", text)
	}
	return out
}

func mainFile(srv *mocktest.Server, path string) string {
	st := srv.Get("/_mock/repos/acme/shop")
	file := st["branches"].(map[string]any)["main"].(map[string]any)["files"].(map[string]any)[path]
	if file == nil {
		return ""
	}
	return file.(map[string]any)["content"].(string)
}

func TestForgeOpenMergeDeployRevert(t *testing.T) {
	srv, call := forgeSetup(t)
	opened := forgeOpen(t, call)
	ref := opened["merge_request"].(string)
	head := opened["head_sha"].(string)
	if ref != "acme/shop#1" || len(head) != 40 {
		t.Fatalf("open result: %v", opened)
	}

	_, _, st := call("merge_request_state", map[string]any{"merge_request": ref})
	if st["state"] != "open" || st["pipeline_green"] != true || st["head_sha"] != head {
		t.Fatalf("state: %v", st)
	}
	files := st["changed_files"].([]any)
	if len(files) != 1 {
		t.Fatalf("changed files: %v", files)
	}
	f := files[0].(map[string]any)
	if f["path"] != "src/db.go" || f["change"] != fixloop.ChangeModified || f["blob_sha"] != fixloop.BlobSHA(fixed) {
		t.Errorf("changed file: %v", f)
	}

	isErr, text, merged := call("merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": head})
	if isErr || merged["merged"] != true || merged["state"] != fixloop.Merged {
		t.Fatalf("merge: %s %v", text, merged)
	}
	if got := mainFile(srv, "src/db.go"); got != fixed {
		t.Errorf("main after merge: %q", got)
	}

	_, _, dep := call("deployment_state", map[string]any{"merge_request": ref})
	if dep["deployed"] != true || dep["application"] != "acme/shop" {
		t.Errorf("deployment: %v", dep)
	}

	isErr, text, rev := call("revert_merge_request", map[string]any{"merge_request": ref})
	if isErr || rev["merged"] != true {
		t.Fatalf("revert: %s %v", text, rev)
	}
	if got := mainFile(srv, "src/db.go"); got != flawed {
		t.Errorf("main after revert: %q", got)
	}
	// A second call finds the work of the first and opens no second pull request.
	if isErr, text, _ = call("revert_merge_request", map[string]any{"merge_request": ref}); isErr {
		t.Errorf("second revert: %s", text)
	}
	if n := len(srv.Get("/_mock/repos/acme/shop")["pulls"].([]any)); n != 2 {
		t.Errorf("pull requests: %d, want 2", n)
	}
}

func TestForgeChangedFilesShowAModeChange(t *testing.T) {
	srv, call := forgeSetup(t)
	ref := forgeOpen(t, call)["merge_request"].(string)
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{
		"branch": "zeroroot/fix-1", "files": map[string]any{"run.sh": map[string]string{"content": "#!/bin/sh\n", "mode": "100755"}},
	})
	_, _, st := call("merge_request_state", map[string]any{"merge_request": ref})
	got := map[string]string{}
	for _, f := range st["changed_files"].([]any) {
		m := f.(map[string]any)
		got[m["path"].(string)] = m["change"].(string) + " " + m["blob_sha"].(string)
	}
	want := map[string]string{
		"src/db.go": fixloop.ChangeModified + " " + fixloop.BlobSHA(fixed),
		"run.sh":    fixloop.ChangeModeChanged + " " + fixloop.BlobSHA("#!/bin/sh\n"),
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: got %q, want %q", p, got[p], w)
		}
	}
}

func TestForgeMergeFailure(t *testing.T) {
	srv, call := forgeSetup(t)
	opened := forgeOpen(t, call)
	srv.Do(http.MethodPut, "/_mock/repos/acme/shop/merge-failure", map[string]any{"status": 405, "message": "Required reviews are missing"})
	_, _, out := call("merge_merge_request", map[string]any{"merge_request": opened["merge_request"], "expected_head_sha": opened["head_sha"]})
	if out["merged"] != false || out["state"] != fixloop.Refused || !strings.Contains(out["reason"].(string), "HTTP 405") {
		t.Errorf("merge with a failure: %v", out)
	}
	srv.Do(http.MethodDelete, "/_mock/repos/acme/shop/merge-failure", nil)
	_, _, out = call("merge_merge_request", map[string]any{"merge_request": opened["merge_request"], "expected_head_sha": opened["head_sha"]})
	if out["merged"] != true {
		t.Errorf("merge after the failure was cleared: %v", out)
	}
}

func TestForgeFailedChecksHoldTheMerge(t *testing.T) {
	srv, call := forgeSetup(t)
	srv.Post("/_mock/repos/acme/shop/checks", map[string]any{"checks": []map[string]string{{"name": "ci", "status": "completed", "conclusion": "failure"}}})
	opened := forgeOpen(t, call)
	_, _, out := call("merge_merge_request", map[string]any{"merge_request": opened["merge_request"], "expected_head_sha": opened["head_sha"]})
	if out["merged"] != false || out["pipeline_green"] != false || out["pipeline_status"] != fixloop.PipelineFailed {
		t.Errorf("merge on red checks: %v", out)
	}
}

func TestForgeCloseAndReadFile(t *testing.T) {
	_, call := forgeSetup(t)
	ref := forgeOpen(t, call)["merge_request"].(string)
	if _, text, out := call("close_merge_request", map[string]any{"merge_request": ref}); out["state"] != fixloop.Closed {
		t.Errorf("close: %s %v", text, out)
	}
	_, _, f := call("read_file", map[string]any{"project": "acme/shop", "ref": "main", "path": "src/db.go"})
	if f["found"] != true || f["content"] != flawed || f["sha"] != fixloop.BlobSHA(flawed) {
		t.Errorf("read_file: %v", f)
	}
	_, _, f = call("read_file", map[string]any{"project": "acme/shop", "ref": "main", "path": "nope.go"})
	if f["found"] != false {
		t.Errorf("missing file: %v", f)
	}
}

func TestForgeMovedHeadRefusesTheMerge(t *testing.T) {
	srv, call := forgeSetup(t)
	opened := forgeOpen(t, call)
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"branch": "zeroroot/fix-1", "files": map[string]string{"extra.go": "package x\n"}})
	_, _, out := call("merge_merge_request", map[string]any{"merge_request": opened["merge_request"], "expected_head_sha": opened["head_sha"]})
	if out["merged"] != false || !strings.Contains(out["reason"].(string), "expected_head_sha") {
		t.Errorf("merge with a moved head: %v", out)
	}
}
