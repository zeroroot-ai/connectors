package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// classCall runs a tool against a Prometheus that answers with one handler, and
// returns the class that the error result carries.
func classCall(t *testing.T, h http.HandlerFunc, tool string, args map[string]any) kit.ErrorClass {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	register, err := build(kit.NewEnvFrom(map[string]string{"PROMETHEUS_BASE_URL": ts.URL}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return kittest.ErrorClass(t, kittest.Call(t, cs, tool, args))
}

func status(code int, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"x","error":"x"}`))
	}
}

var upQuery = map[string]any{"query": "up"}

func TestErrorClassUnavailable(t *testing.T) {
	if c := classCall(t, status(503, nil), "query_instant", upQuery); c.Class != kit.ClassUnavailable {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassTimeout(t *testing.T) {
	if c := classCall(t, status(504, nil), "query_instant", upQuery); c.Class != kit.ClassTimeout {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassRateLimited(t *testing.T) {
	c := classCall(t, status(429, map[string]string{"Retry-After": "120"}), "query_instant", upQuery)
	if c.Class != kit.ClassRateLimited || c.RetryAfterSeconds < 100 || c.RetryAfterSeconds > 120 {
		t.Errorf("class = %+v", c)
	}
}

func TestErrorClassRefused(t *testing.T) {
	// A parse error of the query is a 400 of Prometheus. An empty query is an invalid input.
	if c := classCall(t, status(400, nil), "query_instant", map[string]any{"query": "up(("}); c.Class != kit.ClassRefused {
		t.Errorf("400: %+v", c)
	}
	if c := classCall(t, status(200, nil), "query_instant", map[string]any{"query": ""}); c.Class != kit.ClassRefused {
		t.Errorf("invalid input: %+v", c)
	}
}
