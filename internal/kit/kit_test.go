package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSecretNeverPrints(t *testing.T) {
	s := Secret("hunter2")
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug)
	log.Info("x", "cred", s, "authorization", "Bearer hunter2", "note", "ok")
	b, _ := json.Marshal(map[string]any{"s": s})
	for _, out := range []string{buf.String(), string(b), fmt.Sprintf("%v %+v %#v %s", s, s, s, s)} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("secret leaked: %s", out)
		}
	}
	if !strings.Contains(buf.String(), `"note":"ok"`) {
		t.Errorf("plain attribute was lost: %s", buf.String())
	}
}

func TestEnv(t *testing.T) {
	e := NewEnvFrom(map[string]string{"URL": "https://u:p@h/x", "N": "99", "D": "5s"})
	e.Require("MISSING")
	e.URL("URL")
	e.Int("N", 1, 1, 10)
	if got := e.Duration("D", time.Second); got != 5*time.Second {
		t.Errorf("duration = %v", got)
	}
	err := e.Err()
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{"MISSING", "URL", "N must"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "u:p") {
		t.Errorf("error shows the URL password: %v", err)
	}
}

func TestParseBaseURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://h", "https://", "https://h?q=1", "https://h#f", "https://a:b@h"} {
		if _, err := ParseBaseURL(bad); err == nil {
			t.Errorf("ParseBaseURL(%q) accepted", bad)
		}
	}
	u, err := ParseBaseURL("https://h/api/")
	if err != nil || u.Path != "/api" {
		t.Errorf("got %v, %v", u, err)
	}
}

func TestCheckString(t *testing.T) {
	cases := []struct {
		name string
		v    string
		ok   bool
	}{
		{"ok", "abc-123", true},
		{"empty", "", false},
		{"long", strings.Repeat("a", 65), false},
		{"newline", "a\nb", false},
		{"slash", "a/b", false},
		{"shell", "a;rm -rf", false},
	}
	for _, c := range cases {
		err := CheckString("f", c.v, 64, Alnum)
		if c.name == "ok" {
			err = CheckString("f", "abc123", 64, Alnum)
		}
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: not ErrInvalidInput", c.name)
		}
		if err != nil && strings.Contains(err.Error(), "rm -rf") {
			t.Errorf("%s: error repeats the value", c.name)
		}
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func newTestClient(t *testing.T, h http.Handler, o ClientOptions) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	base, err := ParseBaseURL(ts.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	if o.Sleep == nil {
		o.Sleep = noSleep
	}
	return NewClient(base, o)
}

func TestClientRetriesIdempotentOnly(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"a":1}`))
	})
	c := newTestClient(t, h, ClientOptions{MaxRetries: 2})
	var out struct{ A int }
	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"}, &out); err != nil || out.A != 1 {
		t.Fatalf("GET: %v %+v", err, out)
	}
	if calls.Load() != 3 {
		t.Errorf("GET attempts = %d, want 3", calls.Load())
	}

	calls.Store(0)
	err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/x"}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 503 {
		t.Fatalf("POST error = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("POST attempts = %d, want 1", calls.Load())
	}
}

func TestClientStopsAtRetryLimit(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}), ClientOptions{MaxRetries: 2})
	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"}, nil); err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 3 {
		t.Errorf("attempts = %d, want 3", calls.Load())
	}
}

func TestClientNoRedirectAndEscapedPath(t *testing.T) {
	var gotPath, gotAuth string
	var evil atomic.Bool
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/evil" {
			evil.Store(true)
		}
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		http.Redirect(w, r, "/evil", http.StatusFound)
	}), ClientOptions{Auth: HeaderAuth("Authorization", "Bearer ", "tok")})
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: Path("users", "a/../b?x#y")}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusFound {
		t.Fatalf("err = %v", err)
	}
	if evil.Load() {
		t.Error("client followed a redirect")
	}
	if want := "/base/users/a%2F..%2Fb%3Fx%23y"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestClientLimitsBodyAndErrorSnippet(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/base/err" {
			w.WriteHeader(400)
		}
		_, _ = w.Write([]byte(strings.Repeat("z", 2000)))
	}), ClientOptions{MaxBody: 1000})
	if err := c.Do(context.Background(), Request{Method: "GET", Path: "/big"}, nil); err == nil {
		t.Error("want error for a long body")
	}
	err := c.Do(context.Background(), Request{Method: "GET", Path: "/err"}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || len(ae.Body) > errorSnippet {
		t.Errorf("snippet not cut: %v", err)
	}
}

func TestClientCredentials(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "id" ||
			r.Form.Get("client_secret") != "sec" || r.Form.Get("scope") != "s" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"T","expires_in":3600}`))
	}))
	defer ts.Close()
	now := time.Unix(1000, 0)
	cc := &ClientCredentials{TokenURL: ts.URL, ID: "id", Secret: "sec", ClientSecretInBody: true,
		Extra: map[string][]string{"scope": {"s"}}, Now: func() time.Time { return now }}
	for range 2 {
		tok, err := cc.Token(context.Background())
		if err != nil || tok.Reveal() != "T" {
			t.Fatalf("token = %v, %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("token calls = %d, want 1 (cached)", calls.Load())
	}
	now = now.Add(2 * time.Hour)
	if _, err := cc.Token(context.Background()); err != nil || calls.Load() != 2 {
		t.Errorf("no renewal after expiry: %v %d", err, calls.Load())
	}
	bad := &ClientCredentials{TokenURL: ts.URL, ID: "x", Secret: "sec-leak", ClientSecretInBody: true}
	if _, err := bad.Token(context.Background()); err == nil || strings.Contains(err.Error(), "sec-leak") {
		t.Errorf("error = %v", err)
	}
}

func TestHandlerHealth(t *testing.T) {
	h := NewHandler("t", "1", func(*mcp.Server) {}, slog.New(slog.DiscardHandler))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Errorf("health = %d %s", rr.Code, rr.Body)
	}
}
