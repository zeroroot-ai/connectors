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

// execTree makes the base tree show src/db.go as an executable file.
func execTree(routes map[string]handler, mode string) map[string]handler {
	routes["GET "+repo+"/git/trees/"+srcTree] = reply(200, `{"sha":"`+srcTree+`","truncated":false,"tree":[`+
		`{"path":"db.go","mode":"`+mode+`","type":"blob","sha":"`+blobSHA+`"},{"path":"other.go","mode":"100644","type":"blob","sha":"`+blobSHA+`"}]}`)
	return routes
}

func TestOpenKeepsTheExecutableBitOfAnExistingPath(t *testing.T) {
	m, call := setup(t, execTree(openRoutes(), "100755"))
	args := openArgs()
	args["files"] = []map[string]any{
		{"path": "src/db.go", "content": "package db\n"},       // executable in the base tree
		{"path": "src/other.go", "content": "package db\n"},    // regular in the base tree
		{"path": "src/new.go", "content": "package db\n"},      // new path
		{"path": "fresh/dir/new.go", "content": "package x\n"}, // new folder
	}
	if isErr, text, _ := call("open_merge_request", args); isErr {
		t.Fatal(text)
	}
	entries := m.bodies["POST "+repo+"/git/trees"]["tree"].([]any)
	got := map[string]any{}
	for _, e := range entries {
		x := e.(map[string]any)
		got[x["path"].(string)] = x["mode"]
	}
	want := map[string]any{"src/db.go": "100755", "src/other.go": "100644", "src/new.go": "100644", "fresh/dir/new.go": "100644"}
	for p, mode := range want {
		if got[p] != mode {
			t.Errorf("%s mode = %v, want %v", p, got[p], mode)
		}
	}
}

func TestOpenResumeChecksTheKeptMode(t *testing.T) {
	// The base tree has an executable src/db.go, so the commit of the tool holds 100755.
	exec := func(mode string) map[string]handler {
		routes := execTree(branchExists(openRoutes()), "100755")
		ownTree(routes, `{"path":"src/db.go","mode":"`+mode+`","type":"blob","sha":"`+fixloop.BlobSHA("package db\n")+`"}`)
		routes[compareKey] = reply(200, ownCompare())
		routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(newComSHA)+`]`)
		return routes
	}
	m, call := setup(t, exec("100755"))
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != true {
		t.Fatalf("a commit that keeps 100755 was refused: isErr=%v %s %v", isErr, text, out)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a repeat call wrote: %s", c)
		}
	}
	// A commit that dropped the bit is not the commit of this tool.
	_, call = setup(t, exec("100644"))
	if isErr, text, _ := call("open_merge_request", openArgs()); !isErr || !strings.Contains(text, "does not build on it") {
		t.Errorf("a commit that drops the bit was accepted: isErr=%v %s", isErr, text)
	}
}

func TestOpenKeepsTheExecutableBitInTheForge(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{
		"scripts/run.sh": map[string]string{"content": "#!/bin/sh\necho old\n", "mode": "100755"},
		"scripts/doc.md": "text\n",
	}, nil)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITHUB_API_URL": srv.URL, "GITHUB_TOKEN": "mock-token", "GITHUB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	args := map[string]any{
		"project": "acme/shop", "branch": "zeroroot/fix-1", "base_branch": "main", "title": "Fix the script",
		"files": []map[string]any{
			{"path": "scripts/run.sh", "content": "#!/bin/sh\necho new\n"}, {"path": "scripts/doc.md", "content": "more\n"},
			{"path": "scripts/new.sh", "content": "#!/bin/sh\n"},
		},
	}
	res := kittest.Call(t, cs, "open_merge_request", args)
	if res.IsError {
		t.Fatal(kittest.Text(t, res))
	}
	// A second call finds the commit of the first and writes nothing.
	if res = kittest.Call(t, cs, "open_merge_request", args); res.IsError {
		t.Fatalf("the repeat call: %s", kittest.Text(t, res))
	}
	files := srv.Get("/_mock/repos/acme/shop")["branches"].(map[string]any)["zeroroot/fix-1"].(map[string]any)["files"].(map[string]any)
	for path, mode := range map[string]string{"scripts/run.sh": "100755", "scripts/doc.md": "100644", "scripts/new.sh": "100644"} {
		if got := files[path].(map[string]any)["mode"]; got != mode {
			t.Errorf("%s mode = %v, want %v", path, got, mode)
		}
	}
	if st, _ := srv.Do(http.MethodGet, "/repos/acme/shop/pulls/1", nil); st != 200 {
		t.Errorf("no pull request: %d", st)
	}
}
