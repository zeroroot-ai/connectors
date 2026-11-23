package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReadOnly returns the annotations of a tool that reads and changes nothing.
func ReadOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// Reversible returns the annotations of a tool that changes its target and
// that a second tool undoes (ADR-0180). The description of the tool must name
// that second tool. idempotent is true when the same call twice has the same
// effect as one call.
func Reversible(title string, idempotent bool) *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{
		Title: title, DestructiveHint: &no, IdempotentHint: idempotent, OpenWorldHint: &no,
	}
}

// Irreversible returns the annotations of a tool that changes its target and
// that no tool undoes. The description of the tool must say so.
func Irreversible(title string, idempotent bool) *mcp.ToolAnnotations {
	yes, no := true, false
	return &mcp.ToolAnnotations{
		Title: title, DestructiveHint: &yes, IdempotentHint: idempotent, OpenWorldHint: &no,
	}
}

// AddTool registers a tool, and logs the name, the duration and the outcome of
// each call. It never logs the input or the output of a call.
func AddTool[In, Out any](s *mcp.Server, log *slog.Logger, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	name := t.Name
	readOnly := t.Annotations != nil && t.Annotations.ReadOnlyHint
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		start := time.Now()
		ctx, st := newCallContext(ctx)
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					// The log has the tool and the stack, never the arguments.
					log.Error("tool panic", "tool", name, "panic_type", fmt.Sprintf("%T", rec), "stack", string(debug.Stack()))
					res, err = nil, &PanicError{ReadOnly: readOnly}
				}
			}()
			res, out, err = h(ctx, req, in)
		}()
		outcome := "ok"
		if err != nil {
			outcome = "error"
			if errors.Is(err, ErrInvalidInput) {
				outcome = "invalid_input"
			}
			if st.wroteBefore() {
				err = &AfterWriteError{Err: err}
			}
		}
		log.Info("tool call", "tool", name, "outcome", outcome, "duration_ms", time.Since(start).Milliseconds())
		return res, out, err
	})
}
