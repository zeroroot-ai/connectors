package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

const (
	tokenPath   = "/contoso.onmicrosoft.com/oauth2/v2.0/token"
	createPath  = "/admin/windows/updates/deployments"
	graphBody   = `{"error":{"code":"TooManyRequests","message":"slow down"}}`
	graphBody5  = `{"error":{"code":"GatewayTimeout","message":"upstream took too long"}}`
	startArgsJS = "start_patch_deployment"
)

var startArgs = map[string]any{
	"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2,
}

// withWrap sets the layer around the mock for one test.
func withWrap(t *testing.T, w func(http.Handler) http.Handler) {
	t.Helper()
	wrap = w
	t.Cleanup(func() { wrap = func(h http.Handler) http.Handler { return h } })
}

// The token lives one second, so each request fetches a new token. After the
// create of the deployment, the token endpoint fails. The create ran on Graph, so
// a repeat of the call would make a second deployment: the class is outcome_unknown.
func TestTokenRefreshFailsAfterTheCreate(t *testing.T) {
	withWrap(t, func(h http.Handler) http.Handler {
		var created atomic.Bool
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == tokenPath {
				if created.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":1,"access_token":"graph-token"}`))
				return
			}
			h.ServeHTTP(w, r)
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, createPath) {
				created.Store(true)
			}
		})
	})
	_, call, _ := setup(t)
	isErr, text, _ := call(startArgsJS, startArgs)
	if !isErr {
		t.Fatalf("no error: %s", text)
	}
	if got := kittest.ErrorClass(t, kittest.LastResult()); got.Class != kit.ClassOutcomeUnknown {
		t.Errorf("class = %+v, text %s", got, text)
	}
}

// With no request before it, a token failure is safe to repeat.
func TestTokenFailureBeforeAnyWriteIsUnavailable(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		want   string
	}{"503": {503, kit.ClassUnavailable}, "429": {429, kit.ClassRateLimited}, "401": {401, kit.ClassRefused}} {
		withWrap(t, func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					w.WriteHeader(c.status)
					return
				}
				h.ServeHTTP(w, r)
			})
		})
		_, call, _ := setup(t)
		call(startArgsJS, startArgs)
		if got := kittest.ErrorClass(t, kittest.LastResult()); got.Class != c.want {
			t.Errorf("%s: class = %+v, want %s", name, got, c.want)
		}
	}
}

// The text of a Graph error keeps the cause, so the class comes from the status.
func TestGraphErrorBodyKeepsTheClass(t *testing.T) {
	respond := func(method, suffix string, status int, body string, headers map[string]string) func(http.Handler) http.Handler {
		return func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == method && strings.HasSuffix(r.URL.Path, suffix) {
					for k, v := range headers {
						w.Header().Set(k, v)
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
					return
				}
				h.ServeHTTP(w, r)
			})
		}
	}
	// A Graph 429 with a body is rate_limited, with the time of Retry-After.
	withWrap(t, respond(http.MethodGet, "/deployments/"+depID, 429, graphBody, map[string]string{"Retry-After": "120"}))
	_, call, _ := setup(t)
	_, text, _ := call("get_patch_deployment", map[string]any{"deployment_id": depID})
	got := kittest.ErrorClass(t, kittest.LastResult())
	if got.Class != kit.ClassRateLimited || got.RetryAfterSeconds < 100 || got.RetryAfterSeconds > 120 || !strings.Contains(text, "Graph answered HTTP 429, TooManyRequests: slow down") {
		t.Errorf("429: %+v, text %s", got, text)
	}
	// A Graph create with a 504 and a body is outcome_unknown, and the text shows the body.
	withWrap(t, respond(http.MethodPost, createPath, 504, graphBody5, nil))
	_, call, _ = setup(t)
	_, text, _ = call(startArgsJS, startArgs)
	got = kittest.ErrorClass(t, kittest.LastResult())
	if got.Class != kit.ClassOutcomeUnknown || !strings.Contains(text, "Graph answered HTTP 504, GatewayTimeout: upstream took too long") {
		t.Errorf("504 on create: %+v, text %s", got, text)
	}
	// A Graph 400 with a body is a refusal.
	withWrap(t, respond(http.MethodPost, createPath, 400, `{"error":{"code":"BadRequest","message":"no"}}`, nil))
	_, call, _ = setup(t)
	call(startArgsJS, startArgs)
	if got := kittest.ErrorClass(t, kittest.LastResult()); got.Class != kit.ClassRefused {
		t.Errorf("400 on create: %+v", got)
	}
}

// wrap lets a test put a layer around the handler of the vendor mock.
var wrap = func(h http.Handler) http.Handler { return h }
