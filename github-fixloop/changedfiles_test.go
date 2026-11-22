package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
)

// The mock follows https://docs.github.com/en/rest/pulls/pulls#list-pull-requests-files:
// a list of {filename, status, sha, previous_filename}. A request with per_page=1
// and page=101 reads the file number 101.
func prFile(name, status, sha, previous string) string {
	p := ""
	if previous != "" {
		p = `,"previous_filename":"` + previous + `"`
	}
	return `{"filename":"` + name + `","status":"` + status + `","sha":"` + sha + `"` + p + `,"additions":1}`
}

func stateRoutes(files func(w http.ResponseWriter, r *http.Request, _ map[string]any)) map[string]handler {
	return merge(checks(greenRun, "pending", 0), map[string]handler{
		"GET " + pr7:            reply(200, openPR("clean")),
		"GET " + pr7 + "/files": files,
	})
}

func TestStateChangedFilesKinds(t *testing.T) {
	list := "[" + strings.Join([]string{
		prFile("a.go", "added", "1111", ""),
		prFile("b.go", "modified", "2222", ""),
		prFile("c.go", "removed", "3333", ""),
		prFile("d.go", "renamed", "4444", "old/d.go"),
		prFile("e.go", "copied", "5555", ""),
		prFile("f.go", "changed", "6666", ""),
	}, ",") + "]"
	m, call := setup(t, stateRoutes(func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		_, _ = w.Write([]byte(list))
	}))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	files, _ := out["changed_files"].([]any)
	if len(files) != 6 || out["changed_files_truncated"] != false || out["changed_files_error"] != nil {
		t.Fatalf("out = %v", out)
	}
	want := []fixloop.ChangedFile{
		{Path: "a.go", Change: "added", BlobSHA: "1111"},
		{Path: "b.go", Change: "modified", BlobSHA: "2222"},
		{Path: "c.go", Change: "deleted", BlobSHA: ""},
		{Path: "d.go", Change: "renamed", PreviousPath: "old/d.go", BlobSHA: "4444"},
		{Path: "e.go", Change: "added", BlobSHA: "5555"},
		{Path: "f.go", Change: "modified", BlobSHA: "6666"},
	}
	for i, w := range want {
		f := files[i].(map[string]any)
		prev, _ := f["previous_path"].(string)
		if f["path"] != w.Path || f["change"] != w.Change || f["blob_sha"] != w.BlobSHA || prev != w.PreviousPath {
			t.Errorf("file %d = %v, want %+v", i, f, w)
		}
	}
	if m.called("GET "+pr7+"/files") != 1 {
		t.Errorf("a short list needed a second call: %v", m.calls)
	}
}

func TestStateChangedFilesTruncated(t *testing.T) {
	full := make([]string, 0, fixloop.MaxChangedFiles)
	for i := range fixloop.MaxChangedFiles {
		full = append(full, prFile("f"+strconv.Itoa(i)+".go", "added", "aa", ""))
	}
	for name, tc := range map[string]struct {
		more string
		want bool
	}{"more files": {`[` + prFile("f100.go", "added", "bb", "") + `]`, true}, "exactly 100": {`[]`, false}} {
		var pages []string
		_, call := setup(t, stateRoutes(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			pages = append(pages, r.URL.RawQuery)
			if r.URL.Query().Get("page") == "101" {
				_, _ = w.Write([]byte(tc.more))
				return
			}
			_, _ = w.Write([]byte("[" + strings.Join(full, ",") + "]"))
		}))
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		files, _ := out["changed_files"].([]any)
		if len(files) != 100 || out["changed_files_truncated"] != tc.want {
			t.Errorf("%s: %d files, truncated %v", name, len(files), out["changed_files_truncated"])
		}
		if len(pages) != 2 || !strings.Contains(pages[1], "per_page=1") {
			t.Errorf("%s: pages = %v", name, pages)
		}
	}
}

// A failed read of the files does not fail the state. The error is in the result,
// so the caller knows that the files are unknown.
func TestStateChangedFilesError(t *testing.T) {
	_, call := setup(t, stateRoutes(func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	files, _ := out["changed_files"].([]any)
	errText, _ := out["changed_files_error"].(string)
	if isErr || out["state"] != "open" || len(files) != 0 || !strings.Contains(errText, "boom") {
		t.Errorf("isErr=%v %s out=%v", isErr, text, out)
	}
}
