package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
)

// The mock follows https://docs.github.com/en/rest/commits/commits#compare-two-commits:
// the answer has files with {filename, status, sha, previous_filename}. The tool
// reads the compare of the base sha and the head sha of the pull request, so the
// route holds both shas.
func prFile(name, status, sha, previous string) string {
	p := ""
	if previous != "" {
		p = `,"previous_filename":"` + previous + `"`
	}
	return `{"filename":"` + name + `","status":"` + status + `","sha":"` + sha + `"` + p + `,"additions":1}`
}

func compareFiles(files ...string) string {
	return `{"status":"ahead","ahead_by":1,"files":[` + strings.Join(files, ",") + `]}`
}

// compareKeyFor is the route of the compare of a base and a head.
func compareKeyFor(base, head string) string {
	return "GET " + repo + "/compare/" + base + "..." + head
}

func stateRoutes(files string) map[string]handler {
	return merge(checks(greenRun, "pending", 0), map[string]handler{
		"GET " + pr7:                    reply(200, openPR("clean")),
		compareKeyFor(baseSHA, headSHA): reply(200, files),
	})
}

func TestStateChangedFilesKinds(t *testing.T) {
	m, call := setup(t, stateRoutes(compareFiles(
		prFile("a.go", "added", "1111", ""),
		prFile("b.go", "modified", "2222", ""),
		prFile("c.go", "removed", "3333", ""),
		prFile("d.go", "renamed", "4444", "old/d.go"),
		prFile("e.go", "copied", "5555", ""),
		prFile("f.go", "changed", "6666", ""),
		prFile("g.go", "unchanged", "7777", ""),
	)))
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	if isErr {
		t.Fatal(text)
	}
	files, _ := out["changed_files"].([]any)
	if len(files) != 7 || out["changed_files_truncated"] != false || out["changed_files_error"] != nil {
		t.Fatalf("out = %v", out)
	}
	want := []fixloop.ChangedFile{
		{Path: "a.go", Change: "added", BlobSHA: "1111"},
		{Path: "b.go", Change: "modified", BlobSHA: "2222"},
		{Path: "c.go", Change: "deleted", BlobSHA: ""},
		{Path: "d.go", Change: "renamed", PreviousPath: "old/d.go", BlobSHA: "4444"},
		{Path: "e.go", Change: "added", BlobSHA: "5555"},
		{Path: "f.go", Change: "mode_changed", BlobSHA: "6666"},
		{Path: "g.go", Change: "modified", BlobSHA: "7777"},
	}
	for i, w := range want {
		f := files[i].(map[string]any)
		prev, _ := f["previous_path"].(string)
		if f["path"] != w.Path || f["change"] != w.Change || f["blob_sha"] != w.BlobSHA || prev != w.PreviousPath {
			t.Errorf("file %d = %v, want %+v", i, f, w)
		}
	}
	// One compare call, with the base sha and the head sha. No call to the files endpoint of the pull request.
	if m.called(compareKeyFor(baseSHA, headSHA)) != 1 || m.called("GET "+pr7+"/files") != 0 {
		t.Errorf("calls = %v", m.calls)
	}
}

// The compare gives at most 300 files. The tool returns 100 and says truncated
// when the answer has more than 100, or when it holds the largest number that
// GitHub gives.
func TestStateChangedFilesTruncated(t *testing.T) {
	for name, tc := range map[string]struct {
		n    int
		want bool
	}{"exactly 100": {100, false}, "101 files": {101, true}, "the limit of 300": {300, true}} {
		files := make([]string, 0, tc.n)
		for i := range tc.n {
			files = append(files, prFile("f"+strconv.Itoa(i)+".go", "added", "aa", ""))
		}
		_, call := setup(t, stateRoutes(compareFiles(files...)))
		_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
		got, _ := out["changed_files"].([]any)
		wantLen := min(tc.n, 100)
		if len(got) != wantLen || out["changed_files_truncated"] != tc.want {
			t.Errorf("%s: %d files, truncated %v", name, len(got), out["changed_files_truncated"])
		}
	}
}

// A failed read of the compare does not fail the state. The error is in the
// result, so the caller knows that the files are unknown.
func TestStateChangedFilesError(t *testing.T) {
	routes := stateRoutes("")
	routes[compareKeyFor(baseSHA, headSHA)] = reply(http.StatusInternalServerError, `{"message":"boom"}`)
	_, call := setup(t, routes)
	isErr, text, out := call("merge_request_state", map[string]any{"merge_request": ref})
	files, _ := out["changed_files"].([]any)
	errText, _ := out["changed_files_error"].(string)
	if isErr || out["state"] != "open" || len(files) != 0 || !strings.Contains(errText, "boom") {
		t.Errorf("isErr=%v %s out=%v", isErr, text, out)
	}
}

// The files are those of the head that the state returned. When the head moves
// to B after that read, or goes A to B and back to A, the compare still names the
// head A and the base of the state, so the files cannot belong to another commit.
// A files endpoint that reads the pull request as it is would pass in the second
// case and lag in the first.
func TestStateChangedFilesArePinnedToTheHead(t *testing.T) {
	const moved = "dddddddddddddddddddddddddddddddddddddddd"
	reads := 0
	routes := stateRoutes("")
	routes["GET "+pr7] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reads++
		head := headSHA
		if reads > 1 { // a read after the state sees another head
			head = moved
		}
		_, _ = w.Write([]byte(pullJSON("open", false, "true", "clean", head)))
	}
	routes[compareKeyFor(baseSHA, headSHA)] = reply(200, compareFiles(prFile("a.go", "added", "1111", "")))
	// A compare of the moved head must not be used.
	routes[compareKeyFor(baseSHA, moved)] = reply(200, compareFiles(prFile("evil.go", "added", "9999", "")))
	m, call := setup(t, routes)
	_, _, out := call("merge_request_state", map[string]any{"merge_request": ref})
	files, _ := out["changed_files"].([]any)
	if out["head_sha"] != headSHA || len(files) != 1 || files[0].(map[string]any)["path"] != "a.go" {
		t.Errorf("out = %v", out)
	}
	if m.called(compareKeyFor(baseSHA, moved)) != 0 || reads != 1 {
		t.Errorf("the tool read the moved head: reads %d calls %v", reads, m.calls)
	}
}
