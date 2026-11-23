package main

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
)

// The mock follows https://docs.gitlab.com/api/repositories/ :
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

// realToken has the shape that Gitaly gives: the standard base64 of
// {"file_name":...,"tree_oid":...}. It has padding, it has + or /, and a long
// path makes it longer than 200 characters.
func realToken() string {
	long := strings.Repeat("deeply/nested/folder-with-a-long-name/", 8) + "file?>>.go"
	raw := `{"file_name":"` + long + `","tree_oid":"` + strings.Repeat("ab", 20) + `"}`
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func TestListTreeKeysetPages(t *testing.T) {
	old := treePage
	treePage = 2
	t.Cleanup(func() { treePage = old })
	tok := realToken()
	if len(tok) < 300 || !strings.HasSuffix(tok, "=") && !strings.ContainsAny(tok, "+/") {
		t.Fatalf("the token has no real shape: %s", tok)
	}
	_, call := setup(t, routeMap{
		"GET " + projectPath: reply(200, projectInfo),
		"GET " + treePath: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			if r.URL.Query().Get("per_page") != "2" {
				http.Error(w, `{"message":"bad per_page"}`, http.StatusBadRequest)
				return
			}
			switch r.URL.Query().Get("page_token") {
			case "":
				w.Header().Set("Link", `<https://gitlab.example.com/api/v4/projects/5/repository/tree?page_token=`+url.QueryEscape(tok)+`&pagination=keyset>; rel="next"`)
				_, _ = w.Write([]byte("[" + treeNode("a1", "a", "blob", "100644") + "," + treeNode("a2", "b", "blob", "100644") + "]"))
			case tok:
				_, _ = w.Write([]byte("[" + treeNode("a3", "c", "blob", "100644") + "]"))
			default:
				http.Error(w, `{"message":"bad token"}`, http.StatusBadRequest)
			}
		},
	})
	bind := fixloop.CursorBinding("acme/shop", "", "main")
	_, _, p1 := call("list_tree", map[string]any{"project": "acme/shop"})
	if p1["next_cursor"] != bind+":"+tok || len(p1["entries"].([]any)) != 2 || p1["truncated"] != false {
		t.Fatalf("page 1 = %v", p1)
	}
	isErr, msg, p2 := call("list_tree", map[string]any{"project": "acme/shop", "cursor": bind + ":" + tok})
	if isErr || p2["next_cursor"] != nil || len(p2["entries"].([]any)) != 1 || p2["truncated"] != false {
		t.Errorf("page 2 = %v %s", p2, msg)
	}
	// A cursor holds the project, the ref and the path of its listing.
	cursor := bind + ":" + tok
	for name, args := range map[string]map[string]any{
		"another project": {"project": "acme/other", "ref": "main", "cursor": cursor},
		"another path":    {"project": "acme/shop", "path": "src", "cursor": cursor},
		"another ref":     {"project": "acme/shop", "ref": "dev", "cursor": cursor},
		"a bare token":    {"project": "acme/shop", "cursor": tok},
		"no token":        {"project": "acme/shop", "cursor": bind + ":"},
	} {
		if isErr, _, _ := call("list_tree", args); !isErr {
			t.Errorf("%s: a bad cursor was accepted", name)
		}
	}
}

// An empty list for a path is no proof of an empty folder. A folder of git is never empty.
func TestListTreeEmptyListForAPath(t *testing.T) {
	parent := func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch {
		case r.URL.Query().Get("recursive") == "true":
			_, _ = w.Write([]byte("[]")) // GitLab before 17.7: a missing folder or a file gives []
		case r.URL.Query().Get("path") == "src":
			_, _ = w.Write([]byte("[" + treeNode("f1", "src/main.go", "blob", "100644") + "," + treeNode("d1", "src/db", "tree", "040000") + "]"))
		case r.URL.Query().Get("path") == "":
			_, _ = w.Write([]byte("[" + treeNode("f2", "top.txt", "blob", "100644") + "," + treeNode("d2", "src", "tree", "040000") + "]"))
		default:
			http.Error(w, `{"message":"404 Tree Not Found"}`, http.StatusNotFound)
		}
	}
	for name, c := range map[string]struct{ path, want string }{
		"a file":                      {"src/main.go", "is a file"},
		"a file at the root":          {"top.txt", "is a file"},
		"a missing folder":            {"src/nope", "is not a folder"},
		"a missing root folder":       {"nope", "is not a folder"},
		"a folder with an empty list": {"src/db", "cannot tell"},
		"a missing parent":            {"x/y/z", "404"},
	} {
		_, call := setup(t, routeMap{"GET " + projectPath: reply(200, projectInfo), "GET " + treePath: parent})
		isErr, msg, out := call("list_tree", map[string]any{"project": "acme/shop", "path": c.path})
		if !isErr || !strings.Contains(msg, c.want) {
			t.Errorf("%s: isErr=%v %s %v", name, isErr, msg, out)
		}
	}
	// With no path, an empty list is an empty repository.
	_, call := setup(t, routeMap{"GET " + projectPath: reply(200, projectInfo), "GET " + treePath: reply(200, "[]")})
	if isErr, _, out := call("list_tree", map[string]any{"project": "acme/shop"}); isErr || out["truncated"] != false {
		t.Errorf("empty repository: %v", out)
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
