package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/mockforge/mocktest"
)

// These tests run list_tree against the stateful mock forge. The mock follows
// https://docs.github.com/en/rest/git/trees#get-a-tree : a recursive answer holds
// the folders too, a blob has a size, and a tree that is too large answers
// truncated true.
func treeForge(t *testing.T) (*mocktest.Server, forgeCall) {
	t.Helper()
	srv, call := forgeSetup(t)
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"files": map[string]any{
		"README.md":      "hello\n",
		"src/main.go":    "package main\n",
		"src/db/db.go":   "package db\n",
		"bin/run.sh":     map[string]string{"content": "#!/bin/sh\n", "mode": "100755"},
		"link":           map[string]string{"content": "src/main.go", "mode": "120000"},
		"vendor/lib":     map[string]string{"content": "", "mode": "160000"},
		"docs/a/b/c.txt": "deep\n",
	}})
	return srv, call
}

func treeEntries(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	m := map[string]map[string]any{}
	for _, e := range out["entries"].([]any) {
		x := e.(map[string]any)
		m[x["path"].(string)] = x
	}
	return m
}

func TestListTreeWholeRepository(t *testing.T) {
	_, call := treeForge(t)
	isErr, text, out := call("list_tree", map[string]any{"project": "acme/shop"})
	if isErr {
		t.Fatal(text)
	}
	if out["ref"] != "main" || out["truncated"] != false || out["next_cursor"] != nil {
		t.Fatalf("out = %v", out)
	}
	m := treeEntries(t, out)
	for path, want := range map[string][2]string{
		"README.md": {fixloop.TreeFile, "100644"}, "src": {fixloop.TreeDir, "040000"}, "src/db": {fixloop.TreeDir, "040000"},
		"src/db/db.go": {fixloop.TreeFile, "100644"}, "bin/run.sh": {fixloop.TreeFile, "100755"},
		"link": {fixloop.TreeSymlink, "120000"}, "vendor/lib": {fixloop.TreeSubmodule, "160000"}, "docs/a/b/c.txt": {fixloop.TreeFile, "100644"},
		"docs/a": {fixloop.TreeDir, "040000"}, "run.sh": {fixloop.TreeFile, "100644"}, "src/main.go": {fixloop.TreeFile, "100644"},
	} {
		e := m[path]
		if e == nil || e["type"] != want[0] || e["mode"] != want[1] {
			t.Errorf("%s = %v, want %v", path, e, want)
		}
	}
	if e := m["README.md"]; e["size"] != float64(6) || e["sha"] != fixloop.BlobSHA("hello\n") {
		t.Errorf("README.md = %v", e)
	}
	if e := m["vendor/lib"]; e["size"] != nil {
		t.Errorf("a submodule has no size: %v", e)
	}
}

func TestListTreePathPrefixAndRef(t *testing.T) {
	_, call := treeForge(t)
	_, _, out := call("list_tree", map[string]any{"project": "acme/shop", "path": "docs/a", "ref": "main"})
	m := treeEntries(t, out)
	if len(m) != 2 || m["docs/a/b"]["type"] != "dir" || m["docs/a/b/c.txt"]["type"] != "file" || out["path"] != "docs/a" {
		t.Errorf("out = %v", out)
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "acme/shop", "path": "README.md"}); !isErr {
		t.Error("a file as path was accepted")
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "acme/shop", "path": "nope"}); !isErr {
		t.Error("a missing folder was accepted")
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "acme/shop", "ref": "nobranch"}); !isErr {
		t.Error("a missing ref was accepted")
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "acme/none"}); !isErr {
		t.Error("a missing project was accepted")
	}
}

func TestListTreeCursorPages(t *testing.T) {
	old := treePage
	treePage = 4
	t.Cleanup(func() { treePage = old })
	_, call := treeForge(t)
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		args := map[string]any{"project": "acme/shop"}
		if cursor != "" {
			args["cursor"] = cursor
		}
		isErr, text, out := call("list_tree", args)
		if isErr {
			t.Fatal(text)
		}
		pages++
		for p := range treeEntries(t, out) {
			if seen[p] {
				t.Errorf("%s twice", p)
			}
			seen[p] = true
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 4 || len(seen) != 16 {
		t.Errorf("pages=%d entries=%d", pages, len(seen))
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "acme/shop", "cursor": "bogus"}); !isErr {
		t.Error("a bad cursor was accepted")
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "acme/shop", "cursor": fmt.Sprintf("%040d:99999", 1)}); !isErr {
		t.Error("a cursor of another tree was accepted")
	}
}

func TestListTreeTruncatedIsReported(t *testing.T) {
	srv, call := treeForge(t)
	srv.Do(http.MethodPatch, "/_mock/repos/acme/shop", map[string]any{"tree_limit": 5})
	isErr, text, out := call("list_tree", map[string]any{"project": "acme/shop"})
	if isErr {
		t.Fatal(text)
	}
	if out["truncated"] != true || out["reason"] == nil || len(out["entries"].([]any)) != 5 {
		t.Errorf("out = %v", out)
	}
	// A smaller folder is complete.
	_, _, sub := call("list_tree", map[string]any{"project": "acme/shop", "path": "docs"})
	if sub["truncated"] != false || len(sub["entries"].([]any)) != 3 {
		t.Errorf("sub = %v", sub)
	}
}
