package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The classes of a tool error. A caller reads the class from the structured
// content of the error result and never from its text.
const (
	// ClassUnavailable is a failure that the same call can pass later, and that a
	// repeat is safe for: the vendor does not answer, or it answers 502 or 503, for
	// a request that is idempotent or that a tool proved safe to repeat.
	ClassUnavailable = "unavailable"
	// ClassTimeout is a bound that ended, for a request that a repeat is safe for.
	ClassTimeout = "timeout"
	// ClassRateLimited is a refusal by the rate limit of the vendor. The vendor did
	// not run the request, so a later call can pass.
	ClassRateLimited = "rate_limited"
	// ClassOutcomeUnknown is the loss of the answer of a request that is not safe
	// to repeat. The vendor may have run it. A caller never repeats it. It shows
	// the failure and a person checks the vendor.
	ClassOutcomeUnknown = "outcome_unknown"
	// ClassRefused is every other error. The same call fails again.
	ClassRefused = "refused"
)

// MaxRetryAfterSeconds bounds retry_after_seconds in the class of an error.
const MaxRetryAfterSeconds = 3600

// ErrorClass is the class of a tool error. RetryAfterSeconds is the time that
// the vendor asked the caller to wait, at most MaxRetryAfterSeconds. It is 0
// when the vendor named no time.
type ErrorClass struct {
	Class             string `json:"class"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

// RetryableError marks an error that a repeat of the same tool call is proven
// safe for, so the class is unavailable or timeout and not outcome_unknown. It
// keeps the text of the error it wraps. It does not change the class of a
// refusal: the class of the cause comes first, and an HTTP 4xx answer other than
// 429 stays refused. A tool uses it for a failure after which the call leaves a
// state that the next call finishes, for example a branch that stays after the
// pull request failed to open, and for the create of a ticket with a dedup_key.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err as a RetryableError. It returns the concrete type, so the
// caller returns it as an error of its own.
func Retryable(err error) *RetryableError { return &RetryableError{Err: err} }

// Classify returns the class of an error. The cause comes first. The order is:
// the rate limit, an HTTP 4xx answer (refused), the loss of the answer of a
// request that is not safe to repeat (outcome_unknown, unless the tool wrapped
// the error with Retryable), a timeout, an error that a repeat can pass, then
// refused.
func Classify(err error) ErrorClass {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return ErrorClass{Class: ClassRateLimited, RetryAfterSeconds: secondsUntil(rl.Until)}
	}
	var ae *APIError
	hasAPI := errors.As(err, &ae)
	if hasAPI && (ae.RateLimited || ae.Status == http.StatusTooManyRequests) {
		return ErrorClass{Class: ClassRateLimited}
	}
	if hasAPI && ae.Status >= 400 && ae.Status < 500 {
		return ErrorClass{Class: ClassRefused}
	}
	var re *RetryableError
	opted := errors.As(err, &re)
	var ou *OutcomeUnknownError
	if errors.As(err, &ou) && !opted {
		return ErrorClass{Class: ClassOutcomeUnknown}
	}
	switch {
	case isTimeout(err), hasAPI && ae.Status == http.StatusGatewayTimeout:
		return ErrorClass{Class: ClassTimeout}
	case opted, temporary(err):
		return ErrorClass{Class: ClassUnavailable}
	}
	return ErrorClass{Class: ClassRefused}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func secondsUntil(t time.Time) int {
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return int(math.Min(math.Ceil(d.Seconds()), MaxRetryAfterSeconds))
}

// ClassifyErrors makes each error result of a tool carry the structured content
// {"error":{"class":...,"retry_after_seconds":N}}. The text of the result stays
// as the tool wrote it. A panic of a tool becomes an error result of the class
// refused. The panic is logged with the name of the tool and its stack, never
// with the arguments. Call it once for each server, before the first call.
func ClassifyErrors(s *mcp.Server, log *slog.Logger) {
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				tool := ""
				if r, ok := req.(*mcp.CallToolRequest); ok && r.Params != nil {
					tool = r.Params.Name
				}
				log.Error("tool panic", "tool", tool, "panic_type", fmt.Sprintf("%T", rec), "stack", string(debug.Stack()))
				out := &mcp.CallToolResult{}
				out.SetError(errors.New("the tool failed with an internal error"))
				out.StructuredContent = map[string]any{"error": ErrorClass{Class: ClassRefused}}
				res, err = out, nil
			}()
			res, err = next(ctx, method, req)
			if err != nil {
				return res, err
			}
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
				c := ErrorClass{Class: ClassRefused}
				if cause := r.GetError(); cause != nil {
					c = Classify(cause)
				}
				r.StructuredContent = map[string]any{"error": c}
			}
			return res, err
		}
	})
}
