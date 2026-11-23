package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
)

// The mock follows https://docs.gitlab.com/api/repositories/#list-repository-tree :
// an array of {"id","name","type","path","mode"}, the parameters ref, path,
// recursive, pagination=keyset, per_page and page_token, and a Link header with
// rel="next" for the next page. A missing path answers 404.
const treePath = projectPath + "/repository/tree"

func treeNode(id, path, typ, mode string) string {
	name := path[strings.LastIndex(path, "/")+1:]
	return `{"id":"` + id + `","name":"` + name + `","type":"` + typ + `","path":"` + path + `","mode":"` + mode + `"}`
}

var treeFixture = strings.Join([]string{
	treeNode("1111111111111111111111111111111111111111", "README.md", "blob", "100644"),
	treeNode("2222222222222222222222222222222222222222", "src", "tree", "040000"),
	treeNode("3333333333333333333333333333333333333333", "bin/run.sh", "blob", "100755"),
	treeNode("4444444444444444444444444444444444444444", "link", "blob", "120000"),
	treeNode("5555555555555555555555555555555555555555", "vendor/lib", "commit", "160000"),
}, ",")

func TestListTreeTypesAndQuery(t *testing.T) {
	var q url.Values
	m, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + treePath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			q = r.URL.Query()
			_, _ = w.Write([]byte("[" + treeFixture + "]"))
		},
	})
	isErr, text, out := call("list_tree", map[string]any{"project": "acme/shop", "path": "src/"})
	if isErr {
		t.Fatal(text)
	}
	if q.Get("ref") != "main" || q.Get("path") != "src" || q.Get("recursive") != "true" || q.Get("pagination") != "keyset" || q.Get("per_page") != "100" {
		t.Errorf("query = %v", q)
	}
	if out["truncated"] != false || out["next_cursor"] != nil || out["ref"] != "main" || m.token != "glpat-secret" {
		t.Fatalf("out = %v", out)
	}
	got := map[string][2]any{}
	for _, e := range out["entries"].([]any) {
		x := e.(map[string]any)
		got[x["path"].(string)] = [2]any{x["type"], x["mode"]}
		if x["size"] != nil {
			t.Errorf("GitLab gives no size: %v", x)
		}
	}
	for p, want := range map[string][2]any{
		"README.md": {fixloop.TreeFile, "100644"}, "src": {fixloop.TreeDir, "040000"}, "bin/run.sh": {fixloop.TreeFile, "100755"},
		"link": {fixloop.TreeSymlink, "120000"}, "vendor/lib": {fixloop.TreeSubmodule, "160000"},
	} {
		if got[p] != want {
			t.Errorf("%s = %v, want %v", p, got[p], want)
		}
	}
}

func TestListTreeKeysetPages(t *testing.T) {
	old := treePage
	treePage = 2
	t.Cleanup(func() { treePage = old })
	_, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + treePath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			if r.URL.Query().Get("per_page") != "2" {
				http.Error(w, `{"message":"bad per_page"}`, http.StatusBadRequest)
				return
			}
			switch r.URL.Query().Get("page_token") {
			case "":
				w.Header().Set("Link", `<https://gitlab.example.com/api/v4/projects/5/repository/tree?page_token=TOK2&pagination=keyset>; rel="next"`)
				_, _ = w.Write([]byte("[" + treeNode("a1", "a", "blob", "100644") + "," + treeNode("a2", "b", "blob", "100644") + "]"))
			case "TOK2":
				_, _ = w.Write([]byte("[" + treeNode("a3", "c", "blob", "100644") + "]"))
			default:
				http.Error(w, `{"message":"bad token"}`, http.StatusBadRequest)
			}
		},
	})
	_, _, p1 := call("list_tree", map[string]any{"project": "acme/shop"})
	if p1["next_cursor"] != "TOK2" || len(p1["entries"].([]any)) != 2 || p1["truncated"] != false {
		t.Fatalf("page 1 = %v", p1)
	}
	_, _, p2 := call("list_tree", map[string]any{"project": "acme/shop", "cursor": "TOK2"})
	if p2["next_cursor"] != nil || len(p2["entries"].([]any)) != 1 || p2["truncated"] != false {
		t.Errorf("page 2 = %v", p2)
	}
}

func TestListTreeTruncatedAndErrors(t *testing.T) {
	old := treePage
	treePage = 2
	t.Cleanup(func() { treePage = old })
	full := "[" + treeNode("a1", "a", "blob", "100644") + "," + treeNode("a2", "b", "blob", "100644") + "]"
	for name, c := range map[string]struct {
		handler routeHandler
		want    string // "" is a result
	}{
		"a full page and no next link": {reply(200, full), ""},
		"a next link with no token": {func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Link", `<https://gitlab.example.com/x?page=2>; rel="next"`)
			_, _ = w.Write([]byte(full))
		}, "page_token"},
		"a missing path":   {reply(404, `{"message":"404 Tree Not Found"}`), "404"},
		"an unknown type":  {reply(200, `[`+treeNode("a1", "a", "weird", "100644")+`]`), "entry type"},
		"a forbidden call": {reply(403, `{"message":"403 Forbidden"}`), "403"},
	} {
		_, call := setup(t, routeMap{"GET " + projectPath: reply(200, projectInfo), "GET " + treePath: c.handler})
		isErr, msg, out := call("list_tree", map[string]any{"project": "acme/shop"})
		if c.want == "" {
			if isErr || out["truncated"] != true || out["reason"] == nil || out["next_cursor"] != nil {
				t.Errorf("%s: isErr=%v %v", name, isErr, out)
			}
			continue
		}
		if !isErr || !strings.Contains(msg, c.want) {
			t.Errorf("%s: isErr=%v %s", name, isErr, msg)
		}
	}
	// A short page is the last page.
	_, call := setup(t, routeMap{"GET " + projectPath: reply(200, projectInfo), "GET " + treePath: reply(200, `[`+treeNode("a1", "a", "blob", "100644")+`]`)})
	if _, _, out := call("list_tree", map[string]any{"project": "acme/shop"}); out["truncated"] != false {
		t.Errorf("short page: %v", out)
	}
	if isErr, _, _ := call("list_tree", map[string]any{"project": "../x"}); !isErr {
		t.Error("a bad project was accepted")
	}
}
