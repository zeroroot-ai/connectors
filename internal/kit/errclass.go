package kit

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The classes of a tool error. A caller reads the class from the structured
// content of the error result and never from its text.
const (
	// ClassUnavailable is a failure that a later call can pass: the vendor does
	// not answer, or it answers 502 or 503.
	ClassUnavailable = "unavailable"
	// ClassTimeout is a bound that ended: a deadline, a client timeout, or HTTP 504.
	ClassTimeout = "timeout"
	// ClassRateLimited is a refusal by the rate limit of the vendor. A later call
	// can pass.
	ClassRateLimited = "rate_limited"
	// ClassRefused is every other error. The same call fails again.
	ClassRefused = "refused"
)

// ErrorClass is the class of a tool error. RetryAfterSeconds is the time that
// the vendor asked the caller to wait. It is 0 when the vendor named no time.
type ErrorClass struct {
	Class             string `json:"class"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

// RetryableError marks an error that a later call of the same tool can pass. It
// keeps the text of the error it wraps. A tool uses it for a failure after which
// the call leaves a state that the next call finishes, for example a branch that
// stays after the pull request failed to open.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err as a RetryableError.
func Retryable(err error) error { return &RetryableError{Err: err} }

// Classify returns the class of an error. The order is: the rate limit, a
// timeout, an error that a later call can pass, then refused.
func Classify(err error) ErrorClass {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return ErrorClass{Class: ClassRateLimited, RetryAfterSeconds: secondsUntil(rl.Until)}
	}
	var ae *APIError
	hasAPI := errors.As(err, &ae)
	switch {
	case hasAPI && ae.Status == http.StatusTooManyRequests:
		return ErrorClass{Class: ClassRateLimited}
	case isTimeout(err), hasAPI && ae.Status == http.StatusGatewayTimeout:
		return ErrorClass{Class: ClassTimeout}
	}
	var re *RetryableError
	if errors.As(err, &re) || temporary(err) {
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
	return int(math.Ceil(d.Seconds()))
}

// ClassifyErrors makes each error result of a tool carry the structured content
// {"error":{"class":...,"retry_after_seconds":N}}. The text of the result stays
// as the tool wrote it. Call it once for each server, before the first call.
func ClassifyErrors(s *mcp.Server) {
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/call" || err != nil {
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
