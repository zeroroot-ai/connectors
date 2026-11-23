package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Default limits of the vendor HTTP client.
const (
	DefaultTimeout    = 20 * time.Second
	DefaultMaxRetries = 2
	DefaultMaxBody    = 8 << 20 // bytes of a response that the client reads
	maxBackoff        = 4 * time.Second
	maxRateWait       = 15 * time.Second // longest wait that a vendor asks for with Retry-After or a reset time
	errorSnippet      = 512              // bytes of a vendor error body in an APIError
)

// ErrAnswerTooLarge is the error of an answer that is longer than the limit of
// the Client. A caller can ask for less and call again.
var ErrAnswerTooLarge = errors.New("vendor answer is too large")

// Authenticator adds the credential to a request. It runs once for each
// attempt, so a token that expires between two attempts is renewed.
type Authenticator func(ctx context.Context, req *http.Request) error

// ClientOptions sets the limits of a Client. A zero field takes its default.
type ClientOptions struct {
	Timeout    time.Duration // for one attempt
	MaxRetries int           // retries after the first attempt. Negative means none.
	MaxBody    int64         // bytes read from a response
	Auth       Authenticator
	Logger     *slog.Logger
	// HTTPClient replaces the transport. Tests set it. The Client always
	// refuses a redirect, whatever the HTTPClient says.
	HTTPClient *http.Client
	// Sleep replaces the wait between two attempts. Tests set it.
	Sleep func(context.Context, time.Duration) error
}

// Client calls one vendor API at one fixed base URL. A caller gives a path
// and never a URL, so an input of a model cannot send the credential to
// another host. The Client never follows a redirect.
type Client struct {
	base       *url.URL
	hc         *http.Client
	auth       Authenticator
	log        *slog.Logger
	maxRetries int
	maxBody    int64
	sleep      func(context.Context, time.Duration) error
}

// NewClient returns a Client for the vendor API at base.
func NewClient(base *url.URL, o ClientOptions) *Client {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	c := *hc // copy, so the caller keeps its own client unchanged
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	c.Timeout = o.Timeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	switch {
	case o.MaxRetries == 0:
		o.MaxRetries = DefaultMaxRetries
	case o.MaxRetries < 0:
		o.MaxRetries = 0
	}
	if o.MaxBody <= 0 {
		o.MaxBody = DefaultMaxBody
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	return &Client{base: base, hc: &c, auth: o.Auth, log: o.Logger,
		maxRetries: o.MaxRetries, maxBody: o.MaxBody, sleep: o.Sleep}
}

// Request describes one vendor API call.
type Request struct {
	Method string
	// Path is the path below the base URL. Build it with Path so that each
	// segment from an input is escaped.
	Path   string
	Query  url.Values
	Header http.Header
	// JSON is the request body. The Client encodes it.
	JSON any
	// Form is a form body. It is used when JSON is nil.
	Form url.Values
	// Retry allows a retry of a method that is not idempotent by default.
	Retry bool
	// ResponseHeader, when not nil, receives the headers of a 2xx answer. A
	// caller uses it to read a page link.
	ResponseHeader *http.Header
}

// Path joins segments into a path. Each segment is escaped, so a segment
// from an input cannot add a path element, a query or a host.
func Path(segments ...string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// APIError is a non-2xx answer of the vendor API.
type APIError struct {
	Status int
	Body   string // the start of the answer, cut to a short length
	// RateLimited is true when the vendor refused for its rate limit: HTTP 429,
	// or HTTP 403 with the header X-RateLimit-Remaining 0 or the header Retry-After
	// (GitHub). The client reads the headers, never the text of the answer.
	RateLimited bool
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("vendor API answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("vendor API answered HTTP %d: %s", e.Status, e.Body)
}

// Do sends the request and decodes a 2xx JSON answer into out. A nil out
// drops the body. It retries an idempotent request on a transport error and on
// HTTP 429, 502, 503 and 504, up to the retry limit of the Client.
func (c *Client) Do(ctx context.Context, r Request, out any) error {
	data, err := c.DoBytes(ctx, r)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode vendor answer: %w", err)
	}
	return nil
}

// DoBytes is like Do, but it returns the body of a 2xx answer as it is. A
// caller uses it for a vendor API that answers in XML.
//
// A request that the client does not repeat (a method that is not idempotent,
// with no Retry) may have run on the vendor when the answer is lost: a transport
// error, a timeout, HTTP 502, 503 or 504. The error is then an
// OutcomeUnknownError, and a caller must not repeat the request.
func (c *Client) DoBytes(ctx context.Context, r Request) ([]byte, error) {
	data, err := c.doBytes(ctx, r)
	if err != nil && !repeatable(r) && lostAnswer(err) {
		return nil, &OutcomeUnknownError{Err: err}
	}
	if err == nil && !repeatable(r) {
		markWrite(ctx) // a later failure of the same tool call must not be repeated
	}
	return data, err
}

func repeatable(r Request) bool {
	return r.Retry || r.Method == http.MethodGet || r.Method == http.MethodHead ||
		r.Method == http.MethodPut || r.Method == http.MethodDelete
}

// lostAnswer is true for an error after which the vendor may have run the
// request: a transport error, or HTTP 502, 503 or 504. A rate limit is not one:
// the vendor did not run the request.
func lostAnswer(err error) bool {
	var af *authFailure
	if errors.As(err, &af) {
		return false // the request was not sent
	}
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *APIError
	if errors.As(err, &ae) && !ae.RateLimited {
		switch ae.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

func (c *Client) doBytes(ctx context.Context, r Request) ([]byte, error) {
	body, contentType, err := encodeBody(r)
	if err != nil {
		return nil, err
	}
	retryable := repeatable(r)
	for attempt := 0; ; attempt++ {
		data, wait, err := c.once(ctx, r, body, contentType)
		if err == nil {
			return data, nil
		}
		if !retryable {
			var ae *APIError
			if errors.As(err, &ae) && ae.RateLimited && wait > 0 {
				// The vendor did not run the request. The client does not repeat it, so
				// the caller gets the time that the vendor named.
				return nil, &RateLimitError{Until: time.Now().Add(wait), Cause: err}
			}
			return nil, err
		}
		if attempt >= c.maxRetries || ctx.Err() != nil || !temporary(err) {
			return nil, err
		}
		if wait <= 0 {
			wait = min(200*time.Millisecond<<attempt, maxBackoff)
		} else if wait > maxRateWait {
			// The vendor asks for a longer wait than the tool accepts. A retry
			// before that time would fail again, so the call fails at once.
			return nil, &RateLimitError{Until: time.Now().Add(wait), Cause: err}
		}
		c.log.Debug("vendor request failed, retry", "method", r.Method, "attempt", attempt+1)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("wait for retry: %w", err)
		}
	}
}

// OutcomeUnknownError is the error of a request that the client does not repeat
// and whose answer is lost. The request may have run on the vendor. It keeps the
// text of the error that it wraps.
type OutcomeUnknownError struct{ Err error }

func (e *OutcomeUnknownError) Error() string { return e.Err.Error() }
func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

func encodeBody(r Request) (body []byte, contentType string, err error) {
	switch {
	case r.JSON != nil:
		b, err := json.Marshal(r.JSON)
		if err != nil {
			return nil, "", fmt.Errorf("encode request body: %w", err)
		}
		return b, "application/json", nil
	case r.Form != nil:
		return []byte(r.Form.Encode()), "application/x-www-form-urlencoded", nil
	}
	return nil, "", nil
}

func (c *Client) once(ctx context.Context, r Request, body []byte, contentType string) ([]byte, time.Duration, error) {
	u := *c.base
	u.RawPath = escapedJoin(c.base, r.Path)
	plain, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, 0, errors.New("build request path")
	}
	u.Path = plain
	if len(r.Query) > 0 {
		u.RawQuery = r.Query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), rd)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zeroroot-connectors")
	for k, vs := range r.Header {
		req.Header[k] = vs
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.auth != nil {
		if err := c.auth(ctx, req); err != nil {
			return nil, 0, &authFailure{err: fmt.Errorf("authenticate: %w", err)}
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The error of net/http can hold the URL. The URL holds no credential,
		// because the credential travels in a header.
		return nil, 0, &transportError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, 0, &transportError{err: err}
	}
	failed := resp.StatusCode < 200 || resp.StatusCode > 299
	if !failed && int64(len(data)) > c.maxBody {
		return nil, 0, fmt.Errorf("%w: it is longer than %d bytes", ErrAnswerTooLarge, c.maxBody)
	}
	if failed {
		snippet := string(data)
		if len(snippet) > errorSnippet {
			snippet = snippet[:errorSnippet]
		}
		return nil, retryAfter(resp), &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(snippet), RateLimited: rateLimited(resp)}
	}
	if r.ResponseHeader != nil {
		*r.ResponseHeader = resp.Header.Clone()
	}
	return data, 0, nil
}

func escapedJoin(base *url.URL, p string) string {
	return strings.TrimRight(base.EscapedPath(), "/") + p
}

// authFailure is the failure to get the credential of a request. The request was
// not sent, so a repeat is safe. It keeps the text of the error that it wraps.
type authFailure struct{ err error }

func (e *authFailure) Error() string { return e.err.Error() }
func (e *authFailure) Unwrap() error { return e.err }

type transportError struct{ err error }

func (e *transportError) Error() string { return "vendor API is not reachable: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func temporary(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.RateLimited {
			return true
		}
		switch ae.Status {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// RateLimitError is the error of a call that the vendor refused with HTTP 429
// and asked to repeat after a time that is longer than the client waits. The
// client does not retry. Until is the time that the vendor named.
type RateLimitError struct {
	Until time.Time
	Cause error
}

func (e *RateLimitError) Error() string {
	return "rate limited until " + e.Until.UTC().Format(time.RFC3339) + ": " + e.Cause.Error()
}

func (e *RateLimitError) Unwrap() error { return e.Cause }

// retryAfter returns the wait that the vendor asks for. It reads Retry-After
// (seconds). On a 429 with no Retry-After it reads X-Rate-Limit-Reset, the
// time of the reset in epoch seconds, which Okta sends. The caller caps the
// wait. A reset time in the past gives no wait, so the backoff applies.
func retryAfter(resp *http.Response) time.Duration {
	now := serverNow(resp)
	ra := strings.TrimSpace(resp.Header.Get("Retry-After"))
	sec, err := strconv.Atoi(ra)
	if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(ra, "-") {
		sec, err = maxRetryAfterSeconds, nil // a number too large for an int
	}
	if err == nil && sec >= 0 {
		// Bound the number before the multiplication, so a huge value cannot
		// overflow the duration.
		return time.Duration(min(sec, maxRetryAfterSeconds)) * time.Second
	}
	// Retry-After can be an HTTP date. The wait is counted from the Date header
	// of the answer, so a wrong clock of this host does not change it.
	if t, err := http.ParseTime(ra); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		return 0
	}
	if rateLimited(resp) {
		// Okta sends X-Rate-Limit-Reset, GitHub sends X-RateLimit-Reset. Each is the
		// time of the reset in epoch seconds.
		for _, h := range []string{"X-Rate-Limit-Reset", "X-RateLimit-Reset"} {
			if reset, err := strconv.ParseInt(resp.Header.Get(h), 10, 64); err == nil {
				if d := time.Unix(reset, 0).Sub(now); d > 0 {
					return min(d, maxRetryAfterSeconds*time.Second)
				}
			}
		}
	}
	return 0
}

// maxRetryAfterSeconds bounds a wait that a vendor names, in seconds. It is one
// year, far above any wait that a tool accepts.
const maxRetryAfterSeconds = 365 * 24 * 3600

// rateLimited reads the headers of a failed answer. It is true for HTTP 429, and
// for HTTP 403 with X-RateLimit-Remaining 0 or a Retry-After header, which is how
// GitHub refuses for its rate limit. It never reads the text of the answer.
func rateLimited(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0" ||
			strings.TrimSpace(resp.Header.Get("Retry-After")) != ""
	}
	return false
}

// serverNow is the time of the vendor: the Date header of the answer. A reset
// time and a Retry-After date are on the clock of the vendor, so the wait is
// counted on that clock. Without a Date header, the clock of this host is used.
func serverNow(resp *http.Response) time.Time {
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		return t
	}
	return time.Now()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("context done: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// DecodeErrorBody decodes the body of an APIError as JSON. The body is cut to
// a short length, so a long answer does not decode.
func DecodeErrorBody(e *APIError, out any) error {
	if err := json.Unmarshal([]byte(e.Body), out); err != nil {
		return fmt.Errorf("decode vendor error body: %w", err)
	}
	return nil
}
