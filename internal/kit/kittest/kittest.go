// Package kittest holds the test helpers of the connector servers.
package kittest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Connect registers the tools on a new server, connects an in-memory MCP
// client, and returns its session. The session closes at the end of the test.
func Connect(t *testing.T, register func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	kit.ClassifyErrors(srv, slog.New(slog.DiscardHandler))
	register(srv)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// Call calls a tool with an argument map and returns the result.
func Call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	lastMu.Lock()
	last = res
	lastMu.Unlock()
	return res
}

var (
	lastMu sync.Mutex
	last   *mcp.CallToolResult
)

// LastResult returns the result of the last call of Call. A test that uses a
// setup function that gives only the text of a result reads the rest from it.
func LastResult() *mcp.CallToolResult {
	lastMu.Lock()
	defer lastMu.Unlock()
	return last
}

// Text returns the text of the first content item of a result.
func Text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content is %T, want text", res.Content[0])
	}
	return tc.Text
}

// Structured decodes the structured output of a result into out.
func Structured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("encode structured content: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

// Tools returns the listed tools by name.
func Tools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	m := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tl := range res.Tools {
		m[tl.Name] = tl
	}
	return m
}

// AssertReversible fails the test unless the tool is a change that a person
// can undo: not read only, not destructive, and a description that names the
// undo tool (ADR-0180).
func AssertReversible(t *testing.T, tools map[string]*mcp.Tool, name, undo string) {
	t.Helper()
	tl := tools[name]
	if tl == nil || tl.Annotations == nil {
		t.Fatalf("tool %s is missing or has no annotations", name)
	}
	a := tl.Annotations
	if a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("%s: want readOnly=false destructive=false, got %+v", name, a)
	}
	if tools[undo] == nil {
		t.Errorf("%s: undo tool %s does not exist", name, undo)
	}
	if !strings.Contains(tl.Description, undo) {
		t.Errorf("%s: description does not name the undo tool %s", name, undo)
	}
}

// AssertIrreversible fails the test unless the tool is a change that no tool
// undoes: not read only, destructive, and a description that says so.
func AssertIrreversible(t *testing.T, tools map[string]*mcp.Tool, name string) {
	t.Helper()
	tl := tools[name]
	if tl == nil || tl.Annotations == nil {
		t.Fatalf("tool %s is missing or has no annotations", name)
	}
	a := tl.Annotations
	if a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("%s: want readOnly=false destructive=true, got %+v", name, a)
	}
	if !strings.Contains(tl.Description, "No tool undoes") {
		t.Errorf("%s: description must say that no tool undoes the action", name)
	}
}

// AssertIdempotent fails the test unless the idempotent hint has the value want.
func AssertIdempotent(t *testing.T, tools map[string]*mcp.Tool, name string, want bool) {
	t.Helper()
	if tl := tools[name]; tl == nil || tl.Annotations == nil || tl.Annotations.IdempotentHint != want {
		t.Errorf("%s: idempotentHint is not %v", name, want)
	}
}

// ErrorClass decodes the structured content of an error result:
// {"error":{"class":...,"retry_after_seconds":N}}. It fails the test when the
// result is not an error or has no class.
func ErrorClass(t *testing.T, res *mcp.CallToolResult) kit.ErrorClass {
	t.Helper()
	if !res.IsError {
		t.Fatal("the result is not an error")
	}
	var out struct {
		Error *kit.ErrorClass `json:"error"`
	}
	Structured(t, res, &out)
	if out.Error == nil || out.Error.Class == "" {
		t.Fatalf("the error result has no class: %v", res.StructuredContent)
	}
	return *out.Error
}

// LostAnswer returns a wrapper for the handler of a vendor mock. The wrapper
// drops the connection of each request that changes the vendor (POST, PATCH,
// PUT and DELETE) after it read the request, so the caller gets no answer, as in
// a lost answer. It lets a token request through, and it serves each read with
// the mock. A test sets it before it starts the mock.
func LostAnswer(h http.Handler) http.Handler { return LostAnswerExcept(h) }

// LostAnswerExcept is LostAnswer, and it lets through a request whose path has
// one of the texts in pass. A test uses it for a read that the vendor serves with
// a POST, for example a search.
func LostAnswerExcept(h http.Handler, pass ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := r.Method != http.MethodGet && r.Method != http.MethodHead
		for _, p := range pass {
			if strings.Contains(r.URL.Path, p) {
				write = false
			}
		}
		if write && !strings.Contains(r.URL.Path, "token") && !strings.Contains(r.URL.Path, "/oauth") {
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = c.Close()
			}
			return
		}
		h.ServeHTTP(w, r)
	})
}
