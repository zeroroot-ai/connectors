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
func classCall(t *testing.T, routes routeMap, tool string, args map[string]any) (kit.ErrorClass, string) {
	t.Helper()
	ts := httptest.NewServer(&mock{routes: routes})
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITLAB_BASE_URL": ts.URL, "GITLAB_TOKEN": "glpat-secret", "GITLAB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	res := kittest.Call(t, cs, tool, args)
	return kittest.ErrorClass(t, res), kittest.Text(t, res)
}

var readRules = map[string]any{"project": "acme/shop", "ref": "main", "path": "policy/rules.yaml"}

func fileRoute(h func(http.ResponseWriter, *http.Request, map[string]any)) routeMap {
	return routeMap{"GET " + filePath: h}
}

func TestErrorClassUnavailable(t *testing.T) {
	if c, _ := classCall(t, fileRoute(reply(503, `{"message":"down"}`)), "read_file", readRules); c.Class != kit.ClassUnavailable {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassTimeout(t *testing.T) {
	if c, _ := classCall(t, fileRoute(reply(504, `{"message":"gateway timeout"}`)), "read_file", readRules); c.Class != kit.ClassTimeout {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassRateLimited(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limit"}`))
	}
	if c, _ := classCall(t, fileRoute(h), "read_file", readRules); c.Class != kit.ClassRateLimited || c.RetryAfterSeconds < 100 || c.RetryAfterSeconds > 120 {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassRefused(t *testing.T) {
	if c, _ := classCall(t, fileRoute(reply(400, `{"message":"bad"}`)), "read_file", readRules); c.Class != kit.ClassRefused || c.RetryAfterSeconds != 0 {
		t.Errorf("400: %+v", c)
	}
	if c, _ := classCall(t, routeMap{}, "read_file", map[string]any{"project": "acme/shop", "ref": "../x", "path": "a"}); c.Class != kit.ClassRefused {
		t.Errorf("invalid input: %+v", c)
	}
}

// The branch stays when the merge request fails to open, and a call again
// finishes the work. The error is typed, so no text is read.
func TestErrorClassBranchStaysIsUnavailable(t *testing.T) {
	routes := openRoutes()
	routes["POST "+api+"/merge_requests"] = reply(422, `{"message":"Validation Failed"}`)
	c, text := classCall(t, routes, "open_merge_request", openArgs())
	if c.Class != kit.ClassUnavailable || !strings.Contains(text, "stays, call the tool again") {
		t.Errorf("class = %+v, text %s", c, text)
	}
}
