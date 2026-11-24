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
func classCall(t *testing.T, routes routeMap, tool string, args map[string]any) (class kit.ErrorClass, text string) {
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

// wrap lets a test put a layer around the handler of the vendor mock.
var wrap = func(h http.Handler) http.Handler { return h }

func dropped(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
	if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = c.Close()
	}
}

// The branch stays when the merge request fails to open, and a call again finds
// the branch and finishes the work, so a repeat is proven safe. The class of the
// cause comes first: a refusal of GitLab stays a refusal.
func TestErrorClassBranchStays(t *testing.T) {
	for name, c := range map[string]struct {
		h    func(http.ResponseWriter, *http.Request, map[string]any)
		want string
	}{
		"502":           {reply(502, `{"message":"bad gateway"}`), kit.ClassUnavailable},
		"503":           {reply(503, `{"message":"down"}`), kit.ClassUnavailable},
		"504":           {reply(504, `{"message":"timeout"}`), kit.ClassTimeout},
		"a lost answer": {dropped, kit.ClassUnavailable},
		"401":           {reply(401, `{"message":"401 Unauthorized"}`), kit.ClassRefused},
		"403":           {reply(403, `{"message":"403 Forbidden"}`), kit.ClassRefused},
		"404":           {reply(404, `{"message":"404 Not Found"}`), kit.ClassRefused},
		"422":           {reply(422, `{"message":"Validation Failed"}`), kit.ClassRefused},
		"429":           {reply(429, `{"message":"slow down"}`), kit.ClassRateLimited},
	} {
		routes := openRoutes()
		routes["POST "+api+"/merge_requests"] = c.h
		cl, text := classCall(t, routes, "open_merge_request", openArgs())
		if cl.Class != c.want {
			t.Errorf("%s: class = %+v, want %s, text %s", name, cl, c.want, text)
		}
		checkStaysText(t, name, text, c.want == kit.ClassRefused)
	}
}

// The answer of a write is lost. The write may have run, and the tool did not
// prove that a repeat is safe, so the class is outcome_unknown. The revert commit
// is one of them. A write that the client repeats because the method is
// idempotent is unavailable.
func TestErrorClassLostAnswerOfEachWriteTool(t *testing.T) {
	routes := openRoutes()
	routes["POST "+api+"/repository/commits"] = dropped
	if cl, text := classCall(t, routes, "open_merge_request", openArgs()); cl.Class != kit.ClassOutcomeUnknown {
		t.Errorf("open_merge_request commit: %+v %s", cl, text)
	}
	routes = revertRoutes(`[]`, revertMRJSON)
	routes["POST "+api+"/repository/commits/"+mergeSHA+"/revert"] = dropped
	if cl, text := classCall(t, routes, "revert_merge_request", map[string]any{"merge_request": ref}); cl.Class != kit.ClassOutcomeUnknown {
		t.Errorf("revert_merge_request revert commit: %+v %s", cl, text)
	}
	routes = revertRoutes(`[]`, revertMRJSON)
	routes["POST "+api+"/repository/branches"] = dropped
	if cl, text := classCall(t, routes, "revert_merge_request", map[string]any{"merge_request": ref}); cl.Class != kit.ClassOutcomeUnknown {
		t.Errorf("revert_merge_request branch: %+v %s", cl, text)
	}
	// close_merge_request: a PUT that sets the state, which the client repeats.
	routes = routeMap{"GET " + api + mr7: reply(200, openMR("success", "mergeable")), "PUT " + api + mr7: dropped}
	if cl, text := classCall(t, routes, "close_merge_request", map[string]any{"merge_request": ref}); cl.Class != kit.ClassUnavailable {
		t.Errorf("close_merge_request: %+v %s", cl, text)
	}
	// close_merge_request with a comment: the note is a POST that the client does not repeat.
	routes = routeMap{"GET " + api + mr7: reply(200, openMR("success", "mergeable")), "POST " + api + mr7 + "/notes": dropped}
	if cl, text := classCall(t, routes, "close_merge_request", map[string]any{"merge_request": ref, "comment": "c"}); cl.Class != kit.ClassOutcomeUnknown {
		t.Errorf("close_merge_request comment: %+v %s", cl, text)
	}
	// merge_merge_request: a PUT with the head sha, which the client repeats.
	routes = routeMap{"GET " + api: reply(200, projectMerge), "GET " + api + mr7: reply(200, openMR("success", "mergeable")), "PUT " + api + mr7 + "/merge": dropped}
	if cl, text := classCall(t, routes, "merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": headSHA}); cl.Class != kit.ClassUnavailable {
		t.Errorf("merge_merge_request: %+v %s", cl, text)
	}
}

// GitLab refuses the create of a merge request with 409 or 422 when one exists, for
// example from a create whose answer was lost. The tool lists the merge requests of
// the branch again and returns the open one with existing true.
func TestOpenReturnsTheExistingMergeRequestAfterAConflict(t *testing.T) {
	for _, status := range []int{409, 422} {
		routes := openRoutes()
		routes["GET "+api+"/merge_requests"] = reply(200, `[`+newMR()+`]`)
		routes["POST "+api+"/merge_requests"] = reply(status, `{"message":["Another open merge request already exists"]}`)
		_, call := setup(t, routes)
		isErr, text, out := call("open_merge_request", openArgs())
		if isErr || out["existing"] != true {
			t.Errorf("status %d: isErr=%v %s %v", status, isErr, text, out)
		}
	}
}

// A conflict with no merge request to find stays a refusal of the create.
func TestOpenConflictWithNoMergeRequestToFindIsRefused(t *testing.T) {
	routes := openRoutes()
	routes["POST "+api+"/merge_requests"] = reply(422, `{"message":"Validation Failed"}`)
	cl, text := classCall(t, routes, "open_merge_request", openArgs())
	if cl.Class != kit.ClassRefused {
		t.Errorf("%+v %s", cl, text)
	}
	checkStaysText(t, "no merge request to find", text, true)
}

// checkStaysText checks the text of a failure to open the request after the branch
// was made. The branch stays in each case. A refusal (HTTP 4xx other than 429) does
// not tell the caller to call the tool again, because it fails the same way.
func checkStaysText(t *testing.T, name, text string, refusal bool) {
	t.Helper()
	if !strings.Contains(text, "the branch "+srcBranch+" stays") {
		t.Errorf("%s: the text does not say that the branch stays: %s", name, text)
	}
	if again := strings.Contains(text, "call the tool again"); again == refusal {
		t.Errorf("%s: call the tool again = %v for a refusal = %v: %s", name, again, refusal, text)
	}
}

// The head of the request that the tool opened is not the commit that it made. The
// text names the open request and says that the branch stays. It does not say that
// nothing was merged, because a request is open now.
func TestOpenHeadMismatchNamesTheOpenRequest(t *testing.T) {
	moved := strings.ReplaceAll(newMR(), headSHA, "9999999999999999999999999999999999999999")
	routes := openRoutes()
	routes["POST "+api+"/merge_requests"] = reply(201, moved)
	_, text := classCall(t, routes, "open_merge_request", openArgs())
	check := func(name, text string) {
		t.Helper()
		for _, want := range []string{"!7", "/merge_requests/7", "is open", "the branch " + srcBranch + " stays"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the text has no %q: %s", name, want, text)
			}
		}
		if strings.Contains(text, "Nothing was merged") {
			t.Errorf("%s: the text says that nothing was merged, but a request is open: %s", name, text)
		}
	}
	check("create", text)
	// The request exists already, on the resume path.
	routes = ownBranch(openRoutes())
	routes["GET "+api+"/merge_requests"] = reply(200, `[`+moved+`]`)
	_, text = classCall(t, routes, "open_merge_request", openArgs())
	check("resume", text)
}
