package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
	"github.com/zeroroot-ai/connectors/internal/mockforge/mocktest"
)

// These tests run the health_signal tool against the stateful mock forge
// (internal/mockforge). The test sets the health value, as the exit test of the
// fix loop does after a deploy.

func forgeHealth(t *testing.T) (srv *mocktest.Server, read func(since time.Time, hold int) map[string]any) {
	t.Helper()
	srv = mocktest.Start(t)
	register, err := build(kit.NewEnvFrom(map[string]string{
		"PROMETHEUS_BASE_URL":       srv.URL,
		"PROMETHEUS_HEALTH_QUERIES": `{"acme/shop":"up{job=\"shop\"} > bool 0"}`,
	}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return srv, func(since time.Time, hold int) map[string]any {
		t.Helper()
		res := kittest.Call(t, cs, "health_signal", map[string]any{
			"application": "acme/shop", "since": since.UTC().Format(time.RFC3339), "hold_seconds": hold,
		})
		if res.IsError {
			t.Fatalf("health_signal: %s", kittest.Text(t, res))
		}
		var out map[string]any
		kittest.Structured(t, res, &out)
		return out
	}
}

func TestForgeHealthyByDefault(t *testing.T) {
	_, read := forgeHealth(t)
	out := read(time.Now().Add(-2*time.Minute), 60)
	if out["known"] != true || out["done"] != true || out["held"] != true {
		t.Errorf("healthy service: %v", out)
	}
}

func TestForgeHealthBrokenAfterADeploy(t *testing.T) {
	srv, read := forgeHealth(t)
	since := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	srv.Post("/_mock/prometheus/health", map[string]any{"value": 1})
	srv.Post("/_mock/prometheus/health", map[string]any{"value": 0, "at": since.Add(30 * time.Second).Format(time.RFC3339)})
	out := read(since, 60)
	if out["done"] != true || out["held"] != false || out["value"] != "0" {
		t.Errorf("broken service: %v", out)
	}
	// The hold time of one hour still runs, and the first 0 ends it at once.
	if out = read(since, 3600); out["done"] != true || out["held"] != false {
		t.Errorf("broken service, long hold: %v", out)
	}
}

func TestForgeHealthStillRunning(t *testing.T) {
	_, read := forgeHealth(t)
	out := read(time.Now().Add(-time.Minute), 3600)
	if out["done"] != false || out["held"] != false {
		t.Errorf("hold time that runs: %v", out)
	}
}

func TestForgeNoData(t *testing.T) {
	srv, read := forgeHealth(t)
	srv.Post("/_mock/prometheus/health", map[string]any{"no_data": true})
	out := read(time.Now().Add(-2*time.Minute), 60)
	if out["done"] != true || out["held"] != false {
		t.Errorf("no data: %v", out)
	}
}

func TestForgeAlerts(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Post("/_mock/prometheus/alerts", map[string]any{"alerts": []map[string]any{
		{"labels": map[string]string{"alertname": "HighErrors"}, "state": "firing"},
	}})
	register, err := build(kit.NewEnvFrom(map[string]string{"PROMETHEUS_BASE_URL": srv.URL}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	var out struct{ Total int }
	kittest.Structured(t, kittest.Call(t, cs, "list_alerts", map[string]any{"alertname": "HighErrors"}), &out)
	if out.Total != 1 {
		t.Errorf("alerts: %d, want 1", out.Total)
	}
}
