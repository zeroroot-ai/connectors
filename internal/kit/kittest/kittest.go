// Package kittest holds the test helpers of the connector servers.
package kittest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connect registers the tools on a new server, connects an in-memory MCP
// client, and returns its session. The session closes at the end of the test.
func Connect(t *testing.T, register func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
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
	return res
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
