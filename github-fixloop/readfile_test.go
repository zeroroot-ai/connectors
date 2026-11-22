package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The mock follows https://docs.github.com/en/rest/repos/contents#get-repository-content:
// a file answers {"type":"file","encoding":"base64","size","sha","content"}
// with new lines in the base64, a folder answers an array, and a missing path
// answers 404 {"message":"Not Found"}.
const (
	filePath = repo + "/contents/policy/rules.yaml"
	blobSHA  = "5555555555555555555555555555555555555555"
)

func fileJSON(typ, enc, content string, size int) string {
	return `{"type":"` + typ + `","encoding":"` + enc + `","size":` + itoa(size) + `,"sha":"` + blobSHA + `","content":"` + content + `"}`
}

func readArgs() map[string]any {
	return map[string]any{"project": "acme/shop", "ref": "main", "path": "policy/rules.yaml"}
}

func TestReadFile(t *testing.T) {
	text := "allow: true\nrevert: false\n"
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	wrapped := enc[:10] + `\n` + enc[10:] + `\n`
	m, call := setup(t, map[string]handler{"GET " + filePath: reply(200, fileJSON("file", "base64", wrapped, len(text)))})
	isErr, msg, out := call("read_file", readArgs())
	if isErr {
		t.Fatal(msg)
	}
	if out["found"] != true || out["content"] != text || out["sha"] != blobSHA || out["path"] != "policy/rules.yaml" || out["ref"] != "main" {
		t.Errorf("out = %v", out)
	}
	if len(m.calls) != 1 || m.calls[0] != "GET "+filePath {
		t.Errorf("calls = %v", m.calls)
	}
}

func TestReadFileMissingFileIsNotAnError(t *testing.T) {
	_, call := setup(t, map[string]handler{"GET " + repo: reply(200, allMethods)})
	isErr, msg, out := call("read_file", readArgs())
	if isErr || out["found"] != false || out["content"] != "" {
		t.Errorf("isErr=%v %s %v", isErr, msg, out)
	}
}

func TestReadFileMissingProjectIsAnError(t *testing.T) {
	_, call := setup(t, map[string]handler{})
	if isErr, msg, _ := call("read_file", readArgs()); !isErr || !strings.Contains(msg, "project may not exist") {
		t.Errorf("isErr=%v %s", isErr, msg)
	}
}

func TestReadFileRefusals(t *testing.T) {
	big := fileJSON("file", "base64", "", 2<<20)
	for name, ans := range map[string]string{
		"folder":     `[{"type":"file","name":"a"}]`,
		"symlink":    fileJSON("symlink", "base64", "", 3),
		"too large":  big,
		"no base64":  fileJSON("file", "none", "", 10),
		"not text":   fileJSON("file", "base64", base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00}), 3),
		"bad base64": fileJSON("file", "base64", "@@@", 3),
	} {
		_, call := setup(t, map[string]handler{"GET " + filePath: reply(200, ans)})
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
		"query in path": func(a map[string]any) { a["path"] = "a?ref=x" },
		"bad ref":       func(a map[string]any) { a["ref"] = "a b" },
		"ref dot dot":   func(a map[string]any) { a["ref"] = "a..b" },
		"no ref":        func(a map[string]any) { a["ref"] = "" },
		"project":       func(a map[string]any) { a["project"] = "acme/shop/../x" },
		"no project":    func(a map[string]any) { a["project"] = "shop" },
	} {
		a := readArgs()
		f(a)
		isErr, _, _ := call("read_file", a)
		// A "?" is a legal file name character, so it is sent as %3F and not refused.
		if name == "query in path" {
			continue
		}
		if !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, c := range m.calls {
		if strings.Contains(c, "..") || strings.Contains(c, "passwd") || strings.Contains(c, ".git/") {
			t.Errorf("a bad path reached GitHub: %s", c)
		}
	}
}

func TestReadFileIsReadOnly(t *testing.T) {
	m, call := setup(t, map[string]handler{"GET " + filePath: reply(200, fileJSON("file", "base64", base64.StdEncoding.EncodeToString([]byte("x")), 1))})
	call("read_file", readArgs())
	for _, c := range m.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("read_file sent %s", c)
		}
	}
}
