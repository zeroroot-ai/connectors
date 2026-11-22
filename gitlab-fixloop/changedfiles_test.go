package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
)

// The mock follows https://docs.gitlab.com/api/merge_requests/#list-merge-request-diffs
// (old_path, new_path, new_file, renamed_file, deleted_file) and
// https://docs.gitlab.com/api/repository_files/ (blob_id). A request with
// per_page=1 and page=101 reads the file number 101.
func blobOf(path string) func(http.ResponseWriter, *http.Request, map[string]any) {
	return reply(200, `{"file_path":"`+path+`","encoding":"base64","blob_id":"blob-`+path+`","content":""}`)
}

func stateWith(files func(http.ResponseWriter, *http.Request, map[string]any), blobs ...string) routeMap {
	routes := routeMap{
		"GET " + api + mr7:            reply(200, openMR("success", "mergeable")),
		"GET " + api + mr7 + "/diffs": files,
	}
	for _, p := range blobs {
		routes["GET "+api+"/repository/files/"+url.PathEscape(p)] = blobOf(p)
	}
	return routes
}

func TestStateChangedFilesKinds(t *testing.T) {
	list := "[" + strings.Join([]string{
		diffJS("a.go", true, false, false),
		diffJS("b.go", false, false, false),
		diffJS("c.go", false, false, true),
		`{"old_path":"old/d.go","new_path":"d.go","new_file":false,"renamed_file":true,"deleted_file":false}`,
	}, ",") + "]"
	m, call := setup(t, stateWith(func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(list)) }, "a.go", "b.go", "d.go"))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	files, _ := out["changed_files"].([]any)
	if len(files) != 4 || out["changed_files_truncated"] != false || out["changed_files_error"] != nil {
		t.Fatalf("out = %v", out)
	}
	want := []fixloop.ChangedFile{
		{Path: "a.go", Change: "added", BlobSHA: "blob-a.go"},
		{Path: "b.go", Change: "modified", BlobSHA: "blob-b.go"},
		{Path: "c.go", Change: "deleted", BlobSHA: ""},
		{Path: "d.go", Change: "renamed", PreviousPath: "old/d.go", BlobSHA: "blob-d.go"},
	}
	for i, w := range want {
		f := files[i].(map[string]any)
		prev, _ := f["previous_path"].(string)
		if f["path"] != w.Path || f["change"] != w.Change || f["blob_sha"] != w.BlobSHA || prev != w.PreviousPath {
			t.Errorf("file %d = %v, want %+v", i, f, w)
		}
	}
	// A deleted file has no blob call, and the blobs are read at the head sha.
	if m.called("GET "+api+"/repository/files/c.go") != 0 {
		t.Error("the tool read the blob of a deleted file")
	}
}

func TestStateChangedFilesTruncated(t *testing.T) {
	var full, paths []string
	for i := range fixloop.MaxChangedFiles {
		p := "f" + strconv.Itoa(i) + ".go"
		paths = append(paths, p)
		full = append(full, diffJS(p, false, false, false))
	}
	for name, tc := range map[string]struct {
		more string
		want bool
	}{"more files": {`[` + diffJS("f100.go", true, false, false) + `]`, true}, "exactly 100": {`[]`, false}} {
		var queries []string
		_, call := setup(t, stateWith(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			queries = append(queries, r.URL.RawQuery)
			if r.URL.Query().Get("page") == "101" {
				_, _ = w.Write([]byte(tc.more))
				return
			}
			_, _ = w.Write([]byte("[" + strings.Join(full, ",") + "]"))
		}, paths...))
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		files, _ := out["changed_files"].([]any)
		if len(files) != 100 || out["changed_files_truncated"] != tc.want {
			t.Errorf("%s: %d files, truncated %v", name, len(files), out["changed_files_truncated"])
		}
		if len(queries) != 2 || !strings.Contains(queries[1], "per_page=1") {
			t.Errorf("%s: queries = %v", name, queries)
		}
	}
}

// A failed read does not fail the state. The error is in the result, so the
// caller knows that the files are unknown.
func TestStateChangedFilesError(t *testing.T) {
	_, call := setup(t, stateWith(func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	files, _ := out["changed_files"].([]any)
	errText, _ := out["changed_files_error"].(string)
	if isErr || out["state"] != "open" || len(files) != 0 || !strings.Contains(errText, "boom") {
		t.Errorf("isErr=%v %s out=%v", isErr, text, out)
	}
	// A missing blob is an error of the list too.
	_, call = setup(t, stateWith(func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		_, _ = w.Write([]byte("[" + diffJS("a.go", true, false, false) + "]"))
	}))
	_, _, out = call("merge_request_state", map[string]any{"merge_request": ref})
	if errText, _ := out["changed_files_error"].(string); errText == "" {
		t.Errorf("out = %v", out)
	}
}
