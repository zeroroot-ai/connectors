package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

func TestGetObject(t *testing.T) {
	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"id":"o1","state":"ok"}`))
	}))
	defer ts.Close()
	env := kit.NewEnvFrom(map[string]string{
		"__ENVPREFIX___BASE_URL": ts.URL, "__ENVPREFIX___TOKEN": "tok",
	})
	register, err := build(env, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)

	res := kittest.Call(t, cs, "get_object", map[string]any{"id": "o1"})
	if res.IsError {
		t.Fatalf("tool error: %s", kittest.Text(t, res))
	}
	var out getObjectOutput
	kittest.Structured(t, res, &out)
	if out.State != "ok" || gotPath != "/api/objects/o1" || gotAuth != "Bearer tok" {
		t.Errorf("out=%+v path=%q auth=%q", out, gotPath, gotAuth)
	}

	// An id with a path element never reaches the vendor.
	gotPath = ""
	if res := kittest.Call(t, cs, "get_object", map[string]any{"id": "../x"}); !res.IsError || gotPath != "" {
		t.Errorf("bad id was not refused: %v %q", res.IsError, gotPath)
	}
}

func TestToolAnnotations(t *testing.T) {
	register, _ := build(kit.NewEnvFrom(map[string]string{
		"__ENVPREFIX___BASE_URL": "http://localhost", "__ENVPREFIX___TOKEN": "x",
	}), slog.New(slog.DiscardHandler))
	tool := kittest.Tools(t, kittest.Connect(t, register))["get_object"]
	if tool == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("get_object must be read only: %+v", tool)
	}
}
