package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want string
		min  int // the least retry_after_seconds
	}{
		"rate limit with a time": {&RateLimitError{Until: time.Now().Add(90 * time.Second), Cause: &APIError{Status: 429}}, ClassRateLimited, 89},
		"rate limit wrapped":     {fmt.Errorf("read: %w", &RateLimitError{Until: time.Now().Add(time.Minute), Cause: errors.New("x")}), ClassRateLimited, 59},
		"429 with no time":       {&APIError{Status: http.StatusTooManyRequests}, ClassRateLimited, 0},
		"502":                    {&APIError{Status: http.StatusBadGateway}, ClassUnavailable, 0},
		"503":                    {&APIError{Status: http.StatusServiceUnavailable}, ClassUnavailable, 0},
		"504":                    {&APIError{Status: http.StatusGatewayTimeout}, ClassTimeout, 0},
		"transport failure":      {&transportError{err: errors.New("connection refused")}, ClassUnavailable, 0},
		"transport timeout":      {&transportError{err: timeoutErr{}}, ClassTimeout, 0},
		"a deadline":             {fmt.Errorf("wait: %w", context.DeadlineExceeded), ClassTimeout, 0},
		"retryable":              {Retryable(fmt.Errorf("the branch stays: %w", &APIError{Status: 422})), ClassUnavailable, 0},
		"retryable over a limit": {Retryable(&RateLimitError{Until: time.Now().Add(time.Minute), Cause: errors.New("x")}), ClassRateLimited, 59},
		"invalid input":          {fmt.Errorf("%w: bad", ErrInvalidInput), ClassRefused, 0},
		"400":                    {&APIError{Status: 400}, ClassRefused, 0},
		"404":                    {&APIError{Status: 404}, ClassRefused, 0},
		"a plain error":          {errors.New("boom"), ClassRefused, 0},
		"canceled":               {context.Canceled, ClassRefused, 0},
	} {
		got := Classify(c.err)
		if got.Class != c.want || got.RetryAfterSeconds < c.min || (c.min == 0 && got.RetryAfterSeconds != 0) {
			t.Errorf("%s: got %+v, want %s and at least %d seconds", name, got, c.want, c.min)
		}
	}
}

func TestRetryableKeepsTheText(t *testing.T) {
	inner := errors.New("open the pull request (the branch x stays, call the tool again): boom")
	if got := Retryable(inner).Error(); got != inner.Error() {
		t.Errorf("text = %q", got)
	}
}

// ClassifyErrors puts the class in the structured content of each error result
// and leaves the text.
func TestClassifyErrorsMiddleware(t *testing.T) {
	type in struct{}
	type out struct {
		OK bool `json:"ok"`
	}
	errs := map[string]error{
		"refused": fmt.Errorf("%w: bad", ErrInvalidInput), "unavailable": &APIError{Status: 503},
		"timeout": context.DeadlineExceeded, "rate_limited": &RateLimitError{Until: time.Now().Add(time.Minute), Cause: errors.New("x")},
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ClassifyErrors(srv)
	for name, e := range errs {
		AddTool(srv, slog.New(slog.DiscardHandler), &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, out, error) {
			return nil, out{}, e
		})
	}
	AddTool(srv, slog.New(slog.DiscardHandler), &mcp.Tool{Name: "fine"}, func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, out, error) {
		return nil, out{OK: true}, nil
	})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	for name, e := range errs {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil || !res.IsError {
			t.Fatalf("%s: err=%v res=%+v", name, err, res)
		}
		sc, _ := res.StructuredContent.(map[string]any)
		ec, _ := sc["error"].(map[string]any)
		if ec["class"] != name {
			t.Errorf("%s: structured content = %v", name, res.StructuredContent)
		}
		if _, ok := ec["retry_after_seconds"]; !ok || (name == "rate_limited") != (ec["retry_after_seconds"].(float64) > 0) {
			t.Errorf("%s: retry_after_seconds = %v", name, ec["retry_after_seconds"])
		}
		if txt := res.Content[0].(*mcp.TextContent).Text; txt != e.Error() {
			t.Errorf("%s: text = %q, want %q", name, txt, e.Error())
		}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "fine", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("fine: %v %+v", err, res)
	}
	if sc, _ := res.StructuredContent.(map[string]any); sc["error"] != nil || sc["ok"] != true {
		t.Errorf("a good result changed: %v", res.StructuredContent)
	}
}
