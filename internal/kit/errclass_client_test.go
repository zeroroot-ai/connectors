package kit

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func client(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(u, ClientOptions{Timeout: 2 * time.Second, Sleep: func(context.Context, time.Duration) error { return nil }})
}

// drop closes the connection with no answer, as a lost answer does.
func drop(w http.ResponseWriter, _ *http.Request) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = c.Close()
	}
}

func status(code int, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
	}
}

func classOf(t *testing.T, c *Client, method string, retry bool) ErrorClass {
	t.Helper()
	err := c.Do(context.Background(), Request{Method: method, Path: "/x", Retry: retry}, nil)
	if err == nil {
		t.Fatal("no error")
	}
	return Classify(err)
}

// A request that the client does not repeat may have run when its answer is
// lost. The class is outcome_unknown. A request that the client repeats is unavailable.
func TestLostAnswerClass(t *testing.T) {
	lost := map[string]http.HandlerFunc{
		"a dropped connection": drop, "502": status(502, nil), "503": status(503, nil), "504": status(504, nil),
	}
	for name, h := range lost {
		c := client(t, h)
		for _, m := range []string{http.MethodPost, http.MethodPatch} {
			if got := classOf(t, c, m, false); got.Class != ClassOutcomeUnknown {
				t.Errorf("%s %s: %+v, want outcome_unknown", m, name, got)
			}
		}
		for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			if got := classOf(t, c, m, false); got.Class != ClassUnavailable && got.Class != ClassTimeout {
				t.Errorf("%s %s: %+v, want unavailable or timeout", m, name, got)
			}
		}
		// A POST that the tool declares idempotent with Retry is repeated, and it is not unknown.
		if got := classOf(t, c, http.MethodPost, true); got.Class == ClassOutcomeUnknown || got.Class == ClassRefused {
			t.Errorf("POST with Retry %s: %+v", name, got)
		}
	}
}

func TestLostAnswerByTimeoutOfTheClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	c := NewClient(u, ClientOptions{Timeout: 50 * time.Millisecond, MaxRetries: -1})
	if got := classOf(t, c, http.MethodPost, false); got.Class != ClassOutcomeUnknown {
		t.Errorf("POST timeout: %+v", got)
	}
	if got := classOf(t, c, http.MethodGet, false); got.Class != ClassTimeout {
		t.Errorf("GET timeout: %+v", got)
	}
}

func TestRefusalsOfAWriteStayRefused(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 422, 500} {
		if got := classOf(t, client(t, status(code, nil)), http.MethodPost, false); got.Class != ClassRefused {
			t.Errorf("POST %d: %+v", code, got)
		}
	}
}

// A 429 on a POST is a rate limit. The vendor did not run the request.
func TestRateLimitOnAWrite(t *testing.T) {
	c := client(t, status(429, map[string]string{"Retry-After": "120"}))
	got := classOf(t, c, http.MethodPost, false)
	if got.Class != ClassRateLimited || got.RetryAfterSeconds < 100 || got.RetryAfterSeconds > 120 {
		t.Errorf("POST 429: %+v", got)
	}
	if got := classOf(t, client(t, status(429, nil)), http.MethodPost, false); got.Class != ClassRateLimited {
		t.Errorf("POST 429 with no time: %+v", got)
	}
}

// GitHub refuses with 403 for its rate limit. The headers say so, not the text.
func TestGitHubRateLimitIs403WithHeaders(t *testing.T) {
	reset := time.Now().Add(90 * time.Second).Unix()
	for name, c := range map[string]struct {
		h    http.HandlerFunc
		want string
	}{
		"remaining 0":             {status(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa64(reset)}), ClassRateLimited},
		"retry after (secondary)": {status(403, map[string]string{"Retry-After": "60"}), ClassRateLimited},
		"a permission refusal":    {status(403, map[string]string{"X-RateLimit-Remaining": "4999"}), ClassRefused},
		"403 with no headers":     {status(403, nil), ClassRefused},
	} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			got := classOf(t, client(t, c.h), m, false)
			if got.Class != c.want {
				t.Errorf("%s %s: %+v, want %s", m, name, got, c.want)
			}
		}
	}
	got := classOf(t, client(t, status(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa64(reset)})), http.MethodGet, false)
	if got.RetryAfterSeconds < 60 || got.RetryAfterSeconds > 91 {
		t.Errorf("retry_after from the reset header: %+v", got)
	}
	// The text of the answer says nothing.
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}
	if got := classOf(t, client(t, h), http.MethodGet, false); got.Class != ClassRefused {
		t.Errorf("the class came from the text: %+v", got)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// A huge Retry-After neither overflows nor passes the bound.
func TestHugeRetryAfterIsBounded(t *testing.T) {
	for _, v := range []string{"99999999", "9999999999999", "99999999999999999999"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			got := classOf(t, client(t, status(429, map[string]string{"Retry-After": v})), m, false)
			if got.Class != ClassRateLimited || got.RetryAfterSeconds <= 0 || got.RetryAfterSeconds > MaxRetryAfterSeconds {
				t.Errorf("%s Retry-After %s: %+v", m, v, got)
			}
		}
	}
	if d := retryAfter(&http.Response{Header: http.Header{"Retry-After": {"9999999999999"}}}); d <= 0 || d > maxRetryAfterSeconds*time.Second {
		t.Errorf("retryAfter = %v", d)
	}
}

// connectTools registers the tools of a test and returns the client session.
func connectTools(t *testing.T, log *slog.Logger, register func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ClassifyErrors(srv, log)
	register(srv)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callClass(t *testing.T, cs *mcp.ClientSession, tool string) (class, text string, res *mcp.CallToolResult) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"secret": "hunter2-token"}})
	if err != nil || !res.IsError {
		t.Fatalf("%s: err=%v res=%+v", tool, err, res)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	ec, _ := sc["error"].(map[string]any)
	class, _ = ec["class"].(string)
	return class, res.Content[0].(*mcp.TextContent).Text, res
}

// A panic of a tool that only reads is a refusal. A panic of a tool that changes
// its target, or of a tool with no annotation, may have changed it before the
// panic, so the class is outcome_unknown. The log has the tool and no argument.
func TestPanicClassFollowsReadOnlyHint(t *testing.T) {
	type in struct {
		Secret string `json:"secret"`
	}
	type out struct{}
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	panics := func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, out, error) {
		boom()
		return nil, out{}, nil
	}
	cs := connectTools(t, log, func(srv *mcp.Server) {
		AddTool(srv, log, &mcp.Tool{Name: "reads", Annotations: ReadOnly("reads")}, panics)
		AddTool(srv, log, &mcp.Tool{Name: "writes", Annotations: Reversible("writes", true)}, panics)
		AddTool(srv, log, &mcp.Tool{Name: "bare"}, panics)
	})
	for tool, want := range map[string]string{"reads": ClassRefused, "writes": ClassOutcomeUnknown, "bare": ClassOutcomeUnknown} {
		class, text, _ := callClass(t, cs, tool)
		if class != want {
			t.Errorf("%s: class = %s, want %s", tool, class, want)
		}
		if strings.Contains(text, "hunter2") || strings.Contains(text, "boom") {
			t.Errorf("%s: the text shows the panic: %s", tool, text)
		}
	}
	if l := logged.String(); !strings.Contains(l, "tool panic") || !strings.Contains(l, "tool=reads") || strings.Contains(l, "hunter2") {
		t.Errorf("log = %s", l)
	}
	// The server still answers after a panic.
	if _, _, res := callClass(t, cs, "reads"); res == nil {
		t.Error("the server stopped")
	}
}

// A tool call that made a request that is not repeatable, and that the vendor
// accepted, must not be repeated after a later failure that a later call could
// pass. The call would repeat the request.
func TestFailureAfterAWriteIsOutcomeUnknown(t *testing.T) {
	var fail atomic.Int32 // the status that the GET answers
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(int(fail.Load()))
			return
		}
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	api := NewClient(u, ClientOptions{Timeout: time.Second, Sleep: func(context.Context, time.Duration) error { return nil }})
	log := slog.New(slog.DiscardHandler)
	type in struct {
		Secret string `json:"secret"`
	}
	type out struct{}
	do := func(ctx context.Context, m, p string) error {
		return api.Do(ctx, Request{Method: m, Path: p}, nil)
	}
	tool := func(steps ...func(context.Context) error) mcp.ToolHandlerFor[in, out] {
		return func(ctx context.Context, _ *mcp.CallToolRequest, _ in) (*mcp.CallToolResult, out, error) {
			for _, s := range steps {
				if err := s(ctx); err != nil {
					return nil, out{}, err
				}
			}
			return nil, out{}, nil
		}
	}
	post := func(ctx context.Context) error { return do(ctx, http.MethodPost, "/w") }
	put := func(ctx context.Context) error { return do(ctx, http.MethodPut, "/w") }
	get := func(ctx context.Context) error { return do(ctx, http.MethodGet, "/r") }
	getAgain := func(ctx context.Context) error { return do(ctx, http.MethodGet, "/r2") }
	badPost := func(ctx context.Context) error { return do(ctx, http.MethodPost, "/bad") }
	opted := func(ctx context.Context) error { return Retryable(get(ctx)) }
	cs := connectTools(t, log, func(srv *mcp.Server) {
		for name, h := range map[string]mcp.ToolHandlerFor[in, out]{
			"read_read":       tool(get, getAgain),
			"post_then_get":   tool(post, get),
			"put_then_get":    tool(put, get),
			"post_then_400":   tool(post, badPost),
			"post_then_opted": tool(post, opted),
		} {
			AddTool(srv, log, &mcp.Tool{Name: name}, h)
		}
	})
	fail.Store(http.StatusServiceUnavailable)
	for tool, want := range map[string]string{
		"read_read":       ClassUnavailable,    // nothing was written
		"post_then_get":   ClassOutcomeUnknown, // the POST ran
		"put_then_get":    ClassUnavailable,    // a PUT is safe to repeat
		"post_then_400":   ClassOutcomeUnknown, // a refusal by a later step must not hide the write
		"post_then_opted": ClassUnavailable,    // the tool proved a repeat safe
	} {
		if class, text, _ := callClass(t, cs, tool); class != want {
			t.Errorf("%s: class = %s, want %s (%s)", tool, class, want, text)
		}
	}
	fail.Store(http.StatusTooManyRequests)
	if class, _, _ := callClass(t, cs, "post_then_get"); class != ClassOutcomeUnknown {
		t.Errorf("a 429 after a write: class = %s", class)
	}
}

func boom() { panic("boom with hunter2-token") }
