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
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		// The blob is read at the head sha and not at a branch name.
		if got := r.URL.Query().Get("ref"); got != headSHA {
			http.Error(w, `{"message":"404 File Not Found (ref `+got+`)"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"file_path":"` + path + `","encoding":"base64","blob_id":"blob-` + path + `","content":""}`))
	}
}

// withCount sets the changes_count of the merge request of a state test.
func withCount(mr, count string) string {
	return strings.Replace(mr, `"changes_count":"1"`, `"changes_count":"`+count+`"`, 1)
}

func stateWith(files func(http.ResponseWriter, *http.Request, map[string]any), blobs ...string) routeMap {
	return stateWithMR(openMR("success", "mergeable"), files, blobs...)
}

func stateWithMR(mr string, files func(http.ResponseWriter, *http.Request, map[string]any), blobs ...string) routeMap {
	routes := routeMap{
		"GET " + api + mr7:            reply(200, mr),
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
	m, call := setup(t, stateWithMR(withCount(openMR("success", "mergeable"), "4"),
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(list)) }, "a.go", "b.go", "d.go"))
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

// changes_count says whether the list is complete: "1000+" and a number that
// differs from the list both mean truncated.
func TestStateChangedFilesTruncated(t *testing.T) {
	full := make([]string, 0, fixloop.MaxChangedFiles)
	paths := make([]string, 0, fixloop.MaxChangedFiles)
	for i := range fixloop.MaxChangedFiles {
		p := "f" + strconv.Itoa(i) + ".go"
		paths = append(paths, p)
		full = append(full, diffJS(p, false, false, false))
	}
	list := "[" + strings.Join(full, ",") + "]"
	for name, tc := range map[string]struct {
		count string
		want  bool
	}{"exactly 100": {"100", false}, "150 changes": {"150", true}, "capped": {"1000+", true}} {
		var queries []string
		_, call := setup(t, stateWithMR(withCount(openMR("success", "mergeable"), tc.count), func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			queries = append(queries, r.URL.RawQuery)
			_, _ = w.Write([]byte(list))
		}, paths...))
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		files, _ := out["changed_files"].([]any)
		if len(files) != 100 || out["changed_files_truncated"] != tc.want || out["changed_files_error"] != nil {
			t.Errorf("%s: %d files, truncated %v, out %v", name, len(files), out["changed_files_truncated"], out)
		}
		if len(queries) != 1 {
			t.Errorf("%s: queries = %v", name, queries)
		}
	}
}

// A diff list that belongs to an earlier head is not shown, and a merge request
// with no count yet gives no list. The state is still returned.
func TestStateChangedFilesOfAnOtherHead(t *testing.T) {
	one := func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		_, _ = w.Write([]byte("[" + diffJS("a.go", false, false, false) + "]"))
	}
	old := "cccccccccccccccccccccccccccccccccccccccc"
	for name, mr := range map[string]string{
		"diff of an earlier head": strings.Replace(openMR("success", "mergeable"), `"head_sha":"`+headSHA+`"}`, `"head_sha":"`+old+`"}`, 1),
		"no count yet":            withCount(openMR("success", "mergeable"), ""),
		"count is no number":      withCount(openMR("success", "mergeable"), "many"),
	} {
		_, call := setup(t, stateWithMR(mr, one, "a.go"))
		isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
		files, _ := out["changed_files"].([]any)
		errText, _ := out["changed_files_error"].(string)
		if isErr || out["state"] != "open" || len(files) != 0 || errText == "" {
			t.Errorf("%s: isErr=%v %s out=%v", name, isErr, text, out)
		}
	}
}

// A file of a diff is read at the head sha, with its mode.
func TestStateChangedFilesModes(t *testing.T) {
	list := "[" + strings.Join([]string{
		diffModeJS("a.go", true, false, false, "", "100755"),
		`{"old_path":"b.go","new_path":"b.go","a_mode":"100644","b_mode":"100755","new_file":false,"renamed_file":false,"deleted_file":false,"diff":""}`,
		`{"old_path":"c.go","new_path":"c.go","a_mode":"100644","b_mode":"100755","new_file":false,"renamed_file":false,"deleted_file":false,"diff":"@@"}`,
	}, ",") + "]"
	_, call := setup(t, stateWithMR(withCount(openMR("success", "mergeable"), "3"),
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { _, _ = w.Write([]byte(list)) }, "a.go", "b.go", "c.go"))
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	files, _ := out["changed_files"].([]any)
	if len(files) != 3 {
		t.Fatalf("out = %v", out)
	}
	want := [][2]string{{"added", "100755"}, {"mode_changed", "100755"}, {"modified", "100755"}}
	for i, w := range want {
		f := files[i].(map[string]any)
		if f["change"] != w[0] || f["mode"] != w[1] {
			t.Errorf("file %d = %v, want %v", i, f, w)
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

// The diffs and the blobs are read after the state. When the head or the diff
// version moves between the reads, the files may belong to another commit, so
// the tool gives none and says why. The state itself is still returned.
func TestStateChangedFilesWhenTheHeadMoves(t *testing.T) {
	const moved = "cccccccccccccccccccccccccccccccccccccccc"
	diffs := func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		_, _ = w.Write([]byte("[" + diffJS("a.go", false, false, false) + "]"))
	}
	for name, second := range map[string]string{
		"the head moves":         strings.Replace(openMR("success", "mergeable"), `"sha":"`+headSHA+`","merge_commit_sha"`, `"sha":"`+moved+`","merge_commit_sha"`, 1),
		"the diff version moves": strings.Replace(openMR("success", "mergeable"), `"head_sha":"`+headSHA+`"}`, `"head_sha":"`+moved+`"}`, 1),
	} {
		reads := 0
		routes := stateWith(diffs, "a.go")
		routes["GET "+api+mr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			reads++
			if reads > 1 { // the read after the files
				_, _ = w.Write([]byte(second))
				return
			}
			_, _ = w.Write([]byte(openMR("success", "mergeable")))
		}
		_, call := setup(t, routes)
		isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
		files, _ := out["changed_files"].([]any)
		errText, _ := out["changed_files_error"].(string)
		if isErr || out["state"] != "open" || out["head_sha"] != headSHA || len(files) != 0 || !strings.Contains(errText, "moved from") {
			t.Errorf("%s: isErr=%v %s out=%v", name, isErr, text, out)
		}
	}
	_, call := setup(t, stateWith(diffs, "a.go"))
	_, _, ok := call("merge_request_state", map[string]any{"merge_request": ref})
	if f, _ := ok["changed_files"].([]any); len(f) != 1 || ok["changed_files_error"] != nil {
		t.Errorf("a stable head: %v", ok)
	}
}
