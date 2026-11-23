package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
	"github.com/zeroroot-ai/connectors/internal/mockforge/mocktest"
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

// dropped closes the connection with no answer, as a lost answer does.
func dropped(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
	if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = c.Close()
	}
}

// The branch stays when the pull request fails to open, and a call again finds
// the branch and finishes the work, so a repeat is proven safe. A failure that
// can pass later is unavailable, even for a POST whose answer is lost. A refusal
// of GitHub stays a refusal: the class of the cause comes first.
func TestErrorClassBranchStays(t *testing.T) {
	for name, c := range map[string]struct {
		h    handler
		want string
	}{
		"502":           {reply(502, `{"message":"bad gateway"}`), kit.ClassUnavailable},
		"503":           {reply(503, `{"message":"down"}`), kit.ClassUnavailable},
		"504":           {reply(504, `{"message":"timeout"}`), kit.ClassTimeout},
		"a lost answer": {dropped, kit.ClassUnavailable},
		"401":           {reply(401, `{"message":"Bad credentials"}`), kit.ClassRefused},
		"403":           {reply(403, `{"message":"Resource not accessible"}`), kit.ClassRefused},
		"404":           {reply(404, `{"message":"Not Found"}`), kit.ClassRefused},
		"422":           {reply(422, `{"message":"Validation Failed"}`), kit.ClassRefused},
		"429":           {reply(429, `{"message":"slow down"}`), kit.ClassRateLimited},
	} {
		for _, resume := range []bool{false, true} {
			routes := openRoutes()
			if resume {
				routes = branchExists(routes)
				routes[compareKey] = reply(200, ownCompare())
				routes["GET "+repo+"/pulls"] = reply(200, `[]`)
			}
			routes["POST "+repo+"/pulls"] = c.h
			cl, text := classCall(t, routes, "open_merge_request", openArgs())
			if cl.Class != c.want {
				t.Errorf("%s (resume %v): class = %+v, want %s, text %s", name, resume, cl, c.want, text)
			}
			checkStaysText(t, name, text, c.want == kit.ClassRefused)
		}
	}
}

// The answer of a write is lost. The write may have run, and the tool did not
// prove that a repeat is safe, so the class is outcome_unknown. A write that
// the client repeats because the method is idempotent is unavailable.
func TestErrorClassLostAnswerOfEachWriteTool(t *testing.T) {
	// open_merge_request: the first write is the blob.
	routes := openRoutes()
	routes["POST "+repo+"/git/blobs"] = dropped
	if cl, _ := classCall(t, routes, "open_merge_request", openArgs()); cl.Class != kit.ClassOutcomeUnknown {
		t.Errorf("open_merge_request blob: %+v", cl)
	}
	// Each other write step of open_merge_request.
	for _, step := range []string{"/git/trees", "/git/commits", "/git/refs"} {
		routes = openRoutes()
		routes["POST "+repo+step] = dropped
		if cl, _ := classCall(t, routes, "open_merge_request", openArgs()); cl.Class != kit.ClassOutcomeUnknown {
			t.Errorf("open_merge_request %s: %+v", step, cl)
		}
	}
	// close_merge_request: a PATCH that sets the state closed, which the client repeats.
	routes = map[string]handler{"GET " + pr7: reply(200, openPR("clean")), "PATCH " + pr7: dropped}
	if cl, text := classCall(t, routes, "close_merge_request", map[string]any{"merge_request": ref}); cl.Class != kit.ClassUnavailable {
		t.Errorf("close_merge_request: %+v %s", cl, text)
	}
	// merge_merge_request: PUT with the head sha, which the client repeats.
	routes = merge(checks(greenRun, "success", 1), map[string]handler{"GET " + pr7: reply(200, openPR("clean")), "PUT " + pr7 + "/merge": dropped})
	cl, text := classCall(t, routes, "merge_merge_request", map[string]any{"merge_request": ref, "expected_head_sha": headSHA})
	if cl.Class != kit.ClassUnavailable {
		t.Errorf("merge_merge_request: %+v %s", cl, text)
	}
}

// wrap lets a test put a layer around the handler of the vendor mock.
var wrap = func(h http.Handler) http.Handler { return h }

// revert_merge_request builds the revert with POST calls. The answer of one is
// lost after a real merge, so the revert may have run: outcome_unknown.
func TestErrorClassLostAnswerOfRevert(t *testing.T) {
	var armed atomic.Bool
	srv := mocktest.StartWrapped(t, func(h http.Handler) http.Handler {
		lost := kittest.LostAnswer(h)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if armed.Load() {
				lost.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	srv.Seed("acme", "shop", map[string]any{"src/db.go": flawed}, nil)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"GITHUB_API_URL": srv.URL, "GITHUB_TOKEN": "mock-token", "GITHUB_ENVIRONMENT": "production",
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	open := kittest.Call(t, cs, "open_merge_request", map[string]any{
		"project": "acme/shop", "branch": "zeroroot/fix-1", "base_branch": "main", "title": "Fix",
		"files": []map[string]any{{"path": "src/db.go", "content": fixed}},
	})
	if open.IsError {
		t.Fatal(kittest.Text(t, open))
	}
	var out map[string]any
	kittest.Structured(t, open, &out)
	head, _ := out["head_sha"].(string)
	if res := kittest.Call(t, cs, "merge_merge_request", map[string]any{"merge_request": out["merge_request"], "expected_head_sha": head}); res.IsError {
		t.Fatal(kittest.Text(t, res))
	}
	armed.Store(true)
	res := kittest.Call(t, cs, "revert_merge_request", map[string]any{"merge_request": out["merge_request"]})
	if got := kittest.ErrorClass(t, res); got.Class != kit.ClassOutcomeUnknown {
		t.Errorf("revert_merge_request: %+v %s", got, kittest.Text(t, res))
	}
}

// GitHub refuses for its rate limit with HTTP 403. The headers say so, not the text.
func TestErrorClassRateLimitedBy403(t *testing.T) {
	limited := func(h map[string]string) handler {
		return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			for k, v := range h {
				w.Header().Set(k, v)
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		}
	}
	reset := strconv.FormatInt(time.Now().Add(8*time.Hour).Unix(), 10)
	for name, c := range map[string]struct {
		h    handler
		want string
	}{
		"remaining 0":          {limited(map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}), kit.ClassRateLimited},
		"retry after":          {limited(map[string]string{"Retry-After": "90"}), kit.ClassRateLimited},
		"a permission refusal": {limited(map[string]string{"X-RateLimit-Remaining": "4000"}), kit.ClassRefused},
	} {
		cl, _ := classCall(t, contentsRoute(c.h), "read_file", readA)
		if cl.Class != c.want {
			t.Errorf("%s: %+v, want %s", name, cl, c.want)
		}
		if c.want == kit.ClassRateLimited && (cl.RetryAfterSeconds < 1 || cl.RetryAfterSeconds > kit.MaxRetryAfterSeconds) {
			t.Errorf("%s: retry_after_seconds = %d", name, cl.RetryAfterSeconds)
		}
	}
}

// GitHub refuses the create of a pull request with 409 or 422 when one exists, for
// example from a create whose answer was lost. The tool lists the pull requests of
// the branch again and returns the open one with existing true.
func TestOpenReturnsTheExistingPullRequestAfterAConflict(t *testing.T) {
	for _, status := range []int{409, 422} {
		for _, resume := range []bool{false, true} {
			routes := openRoutes()
			if resume {
				routes = branchExists(routes)
				routes[compareKey] = reply(200, ownCompare())
				// The first list shows nothing. The pull request exists when the tool lists again.
				lists := 0
				routes["GET "+repo+"/pulls"] = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
					lists++
					if lists == 1 {
						_, _ = w.Write([]byte(`[]`))
						return
					}
					_, _ = w.Write([]byte(`[` + ownPR(newComSHA) + `]`))
				}
			} else {
				routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(newComSHA)+`]`)
			}
			routes["POST "+repo+"/pulls"] = reply(status, `{"message":"A pull request already exists"}`)
			_, call := setup(t, routes)
			isErr, text, out := call("open_merge_request", openArgs())
			if isErr || out["existing"] != true || out["merge_request"] != ref || out["head_sha"] != newComSHA {
				t.Errorf("status %d resume %v: isErr=%v %s %v", status, resume, isErr, text, out)
			}
		}
	}
}

// A conflict with no pull request to find, or one of a fork, or into another
// branch, stays a refusal of the create.
func TestOpenConflictWithNoPullRequestToFindIsRefused(t *testing.T) {
	fork := strings.Replace(ownPR(newComSHA), `"head":{"sha":"`+newComSHA+`","ref":"`+srcBranch+`","repo":{"full_name":"acme/shop"}}`,
		`"head":{"sha":"`+newComSHA+`","ref":"`+srcBranch+`","repo":{"full_name":"evil/shop"}}`, 1)
	for name, list := range map[string]string{
		"none": `[]`, "a fork": `[` + fork + `]`, "closed": `[` + strings.Replace(ownPR(newComSHA), `"state":"open"`, `"state":"closed"`, 1) + `]`,
	} {
		routes := openRoutes()
		routes["GET "+repo+"/pulls"] = reply(200, list)
		routes["POST "+repo+"/pulls"] = reply(422, `{"message":"Validation Failed"}`)
		cl, text := classCall(t, routes, "open_merge_request", openArgs())
		if cl.Class != kit.ClassRefused {
			t.Errorf("%s: %+v %s", name, cl, text)
		}
		checkStaysText(t, name, text, true)
	}
}

// A newer closed pull request of the branch must not hide the open one. The tool
// asks for the open pull requests of the branch into the base branch.
func TestOpenFindsTheOpenPullRequestBesideAClosedOne(t *testing.T) {
	closed := strings.Replace(ownPR(newComSHA), `"state":"open"`, `"state":"closed"`, 1)
	var query url.Values
	listPulls := func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		query = r.URL.Query()
		if r.URL.Query().Get("state") == "open" {
			_, _ = w.Write([]byte(`[` + ownPR(newComSHA) + `]`))
			return
		}
		_, _ = w.Write([]byte(`[` + closed + `]`)) // the newest of all states
	}
	// The resume of an own branch.
	routes := branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = listPulls
	m, call := setup(t, routes)
	isErr, text, out := call("open_merge_request", openArgs())
	if isErr || out["existing"] != true || out["merge_request"] != ref {
		t.Fatalf("resume: isErr=%v %s %v", isErr, text, out)
	}
	if query.Get("state") != "open" || query.Get("base") != "main" || query.Get("head") != "acme:"+srcBranch {
		t.Errorf("query = %v", query)
	}
	if m.called("POST "+repo+"/pulls") != 0 {
		t.Error("the tool opened a second pull request")
	}
	// The conflict of the create.
	routes = openRoutes()
	routes["GET "+repo+"/pulls"] = listPulls
	routes["POST "+repo+"/pulls"] = reply(422, `{"message":"A pull request already exists"}`)
	_, call = setup(t, routes)
	if isErr, text, out := call("open_merge_request", openArgs()); isErr || out["existing"] != true {
		t.Errorf("conflict: isErr=%v %s %v", isErr, text, out)
	}
	// A pull request of the branch into another base branch is not ours.
	other := strings.Replace(ownPR(newComSHA), `"base":{"ref":"main"`, `"base":{"ref":"release"`, 1)
	routes = branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[`+other+`]`)
	routes["POST "+repo+"/pulls"] = reply(201, ownPR(newComSHA))
	m, call = setup(t, routes)
	if isErr, text, out := call("open_merge_request", openArgs()); isErr || out["existing"] == true || m.called("POST "+repo+"/pulls") != 1 {
		t.Errorf("another base: isErr=%v %s %v", isErr, text, out)
	}
}

// The error text of the search for a pull request names the branch, not a revert.
func TestFindPRErrorText(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	t.Cleanup(ts.Close)
	base, err := kit.ParseBaseURL(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	v := &service{client: kit.NewClient(base, kit.ClientOptions{})}
	for name, find := range map[string]func() error{
		"findPR": func() error { _, err := v.findPR(t.Context(), prRef{owner: "acme", repo: "shop"}, "b"); return err },
		"findOpenPR": func() error {
			_, err := v.findOpenPR(t.Context(), prRef{owner: "acme", repo: "shop"}, "b", "main")
			return err
		},
	} {
		err := find()
		if err == nil || strings.Contains(err.Error(), "revert") && name == "findPR" || !strings.Contains(err.Error(), "pull request of the branch") {
			t.Errorf("%s: %v", name, err)
		}
	}
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
	moved := "9999999999999999999999999999999999999999"
	// The create answers a request with another head.
	routes := openRoutes()
	routes["POST "+repo+"/pulls"] = reply(201, ownPR(moved))
	cl, text := classCall(t, routes, "open_merge_request", openArgs())
	checkHeadText(t, "create", cl, text)
	// The request exists already, on the resume path.
	routes = branchExists(openRoutes())
	routes[compareKey] = reply(200, ownCompare())
	routes["GET "+repo+"/pulls"] = reply(200, `[`+ownPR(moved)+`]`)
	cl, text = classCall(t, routes, "open_merge_request", openArgs())
	checkHeadText(t, "resume", cl, text)
}

func checkHeadText(t *testing.T, name string, cl kit.ErrorClass, text string) {
	t.Helper()
	for _, want := range []string{"#7", "https://github.com/acme/shop/pull/7", "is open", "the branch " + srcBranch + " stays"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s: the text has no %q: %s", name, want, text)
		}
	}
	if strings.Contains(text, "Nothing was merged") {
		t.Errorf("%s: the text says that nothing was merged, but a request is open: %s", name, text)
	}
	_ = cl
}
