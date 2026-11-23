package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// classCall runs a tool against the route table and returns the class that the
// error result carries, and its text.
func classCall(t *testing.T, routes map[string]handler, tool string, args map[string]any) (class kit.ErrorClass, text string) {
	t.Helper()
	mergeableWait = 0
	ts := httptest.NewServer(&mock{routes: routes})
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITHUB_API_URL": ts.URL, "GITHUB_TOKEN": "ghp_secret", "GITHUB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	res := kittest.Call(t, cs, tool, args)
	return kittest.ErrorClass(t, res), kittest.Text(t, res)
}

func contentsRoute(h handler) map[string]handler {
	return map[string]handler{"GET " + repo + "/contents/a.txt": h}
}

var readA = map[string]any{"project": "acme/shop", "ref": "main", "path": "a.txt"}

func TestErrorClassUnavailable(t *testing.T) {
	c, _ := classCall(t, contentsRoute(reply(503, `{"message":"down"}`)), "read_file", readA)
	if c.Class != kit.ClassUnavailable {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassTimeout(t *testing.T) {
	c, _ := classCall(t, contentsRoute(reply(504, `{"message":"gateway timeout"}`)), "read_file", readA)
	if c.Class != kit.ClassTimeout {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassRateLimited(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limit"}`))
	}
	c, _ := classCall(t, contentsRoute(h), "read_file", readA)
	if c.Class != kit.ClassRateLimited || c.RetryAfterSeconds < 100 || c.RetryAfterSeconds > 120 {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassRefused(t *testing.T) {
	// A 400 from the host, a missing file for a project that is gone, an invalid
	// input and an overwrite of a symlink are refusals.
	c, _ := classCall(t, contentsRoute(reply(400, `{"message":"bad"}`)), "read_file", readA)
	if c.Class != kit.ClassRefused || c.RetryAfterSeconds != 0 {
		t.Errorf("400: %+v", c)
	}
	c, _ = classCall(t, map[string]handler{}, "read_file", map[string]any{"project": "acme/shop", "ref": "../x", "path": "a.txt"})
	if c.Class != kit.ClassRefused {
		t.Errorf("invalid input: %+v", c)
	}
	c, text := classCall(t, srcEntries(openRoutes(), item("db.go", "120000", "blob")), "open_merge_request", openArgs())
	if c.Class != kit.ClassRefused {
		t.Errorf("symlink overwrite: %+v %s", c, text)
	}
	c, text = classCall(t, srcEntries(openRoutes(), item("db.go", "100644", "blob")), "open_merge_request", map[string]any{
		"project": "acme/shop", "branch": srcBranch, "base_branch": "main", "title": "t",
		"files": []map[string]any{{"path": "src/ghost.go", "delete": true}},
	})
	if c.Class != kit.ClassRefused {
		t.Errorf("delete of a missing path: %+v %s", c, text)
	}
}

// The branch stays when the pull request fails to open, and a call again
// finishes the work. The error is typed, so no text is read.
func TestErrorClassBranchStaysIsUnavailable(t *testing.T) {
	routes := openRoutes()
	routes["POST "+repo+"/pulls"] = reply(422, `{"message":"Validation Failed"}`)
	c, text := classCall(t, routes, "open_merge_request", openArgs())
	if c.Class != kit.ClassUnavailable {
		t.Errorf("class = %+v, text %s", c, text)
	}
	if want := "the branch " + srcBranch + " stays, call the tool again"; !strings.Contains(text, want) {
		t.Errorf("the text changed: %s", text)
	}
}

// The resume of an own branch opens the pull request too, with the same hint.
func TestErrorClassBranchStaysOnResumeIsUnavailable(t *testing.T) {
	routes := branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[]`)
	routes["POST "+repo+"/pulls"] = reply(422, `{"message":"Validation Failed"}`)
	c, text := classCall(t, routes, "open_merge_request", openArgs())
	if c.Class != kit.ClassUnavailable || !strings.Contains(text, "stays, call the tool again") {
		t.Errorf("class = %+v, text %s", c, text)
	}
}
