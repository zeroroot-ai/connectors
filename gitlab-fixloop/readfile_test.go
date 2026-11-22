package main

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The mock follows https://docs.gitlab.com/api/repository_files/#get-file-from-repository:
// the answer is {"file_path","size","encoding":"base64","content","blob_id","ref"}.
// A missing file answers 404 {"message":"404 File Not Found"}. A missing
// project answers 404 {"message":"404 Project Not Found"}.
const (
	filePath = api + "/repository/files/policy%2Frules.yaml"
	blobID   = "7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b"
)

func fileJSON(enc, content string, size int) string {
	return `{"file_path":"policy/rules.yaml","size":` + strconv.Itoa(size) + `,"encoding":"` + enc + `","content":"` + content +
		`","blob_id":"` + blobID + `","ref":"main"}`
}

func readArgs() map[string]any {
	return map[string]any{"project": "acme/shop", "ref": "main", "path": "policy/rules.yaml"}
}

func TestReadFile(t *testing.T) {
	text := "allow: true\nrevert: false\n"
	m, call := setup(t, routeMap{"GET " + filePath: reply(200, fileJSON("base64", base64.StdEncoding.EncodeToString([]byte(text)), len(text)))})
	isErr, msg, out := call("read_file", readArgs())
	if isErr {
		t.Fatal(msg)
	}
	if out["found"] != true || out["content"] != text || out["sha"] != blobID || out["path"] != "policy/rules.yaml" || out["ref"] != "main" {
		t.Errorf("out = %v", out)
	}
	if m.called("GET "+filePath) != 1 || m.token != "glpat-secret" {
		t.Errorf("calls = %v token %q", m.calls, m.token)
	}
}

func TestReadFileMissingFileIsNotAnError(t *testing.T) {
	_, call := setup(t, routeMap{"GET " + filePath: reply(http.StatusNotFound, `{"message":"404 File Not Found"}`)})
	isErr, msg, out := call("read_file", readArgs())
	if isErr || out["found"] != false || out["content"] != "" {
		t.Errorf("isErr=%v %s %v", isErr, msg, out)
	}
}

func TestReadFileMissingProjectIsAnError(t *testing.T) {
	_, call := setup(t, routeMap{"GET " + filePath: reply(http.StatusNotFound, `{"message":"404 Project Not Found"}`)})
	if isErr, msg, _ := call("read_file", readArgs()); !isErr || !strings.Contains(msg, "Project Not Found") {
		t.Errorf("isErr=%v %s", isErr, msg)
	}
}

func TestReadFileRefusals(t *testing.T) {
	for name, ans := range map[string]string{
		"too large":  fileJSON("base64", "", 2<<20),
		"no base64":  fileJSON("text", "x", 1),
		"not text":   fileJSON("base64", base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00}), 3),
		"bad base64": fileJSON("base64", "@@@", 3),
	} {
		_, call := setup(t, routeMap{"GET " + filePath: reply(200, ans)})
		if isErr, _, out := call("read_file", readArgs()); !isErr {
			t.Errorf("%s: no error, out = %v", name, out)
		}
	}
}

func TestReadFileRefusesBadInput(t *testing.T) {
	m, call := setup(t, nil)
	for name, f := range map[string]func(map[string]any){
		"dot dot":       func(a map[string]any) { a["path"] = "policy/../secret" },
		"leading slash": func(a map[string]any) { a["path"] = "/etc/passwd" },
		"git folder":    func(a map[string]any) { a["path"] = ".git/config" },
		"empty path":    func(a map[string]any) { a["path"] = "" },
		"bad ref":       func(a map[string]any) { a["ref"] = "a b" },
		"ref dot dot":   func(a map[string]any) { a["ref"] = "a..b" },
		"no ref":        func(a map[string]any) { a["ref"] = "" },
		"project query": func(a map[string]any) { a["project"] = "acme/shop?x=1" },
		"project dots":  func(a map[string]any) { a["project"] = "acme/../x" },
	} {
		a := readArgs()
		f(a)
		if isErr, _, _ := call("read_file", a); !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a bad input reached GitLab: %v", m.calls)
	}
}
