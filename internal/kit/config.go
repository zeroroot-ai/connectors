package kit

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env reads the configuration of a server from the environment. It collects
// each error, so a server that misses three variables reports all three at
// once. Call Err after the last read.
type Env struct {
	get  func(string) string
	errs []error
}

// NewEnv returns an Env that reads the process environment.
func NewEnv() *Env { return &Env{get: os.Getenv} }

// NewEnvFrom returns an Env that reads from a map. Tests use it.
func NewEnvFrom(m map[string]string) *Env {
	return &Env{get: func(k string) string { return m[k] }}
}

// Err returns every error of the reads so far, or nil.
func (e *Env) Err() error { return errors.Join(e.errs...) }

func (e *Env) fail(format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf(format, args...))
}

// Require returns the value of a variable. A missing or empty variable is an
// error. The error names the variable and never its value.
func (e *Env) Require(name string) string {
	v := strings.TrimSpace(e.get(name))
	if v == "" {
		e.fail("environment variable %s is required", name)
	}
	return v
}

// Optional returns the value of a variable, or def when the variable is empty.
func (e *Env) Optional(name, def string) string {
	if v := strings.TrimSpace(e.get(name)); v != "" {
		return v
	}
	return def
}

// Secret returns the value of a required credential variable.
func (e *Env) Secret(name string) Secret { return Secret(e.Require(name)) }

// OptionalSecret returns the value of an optional credential variable.
func (e *Env) OptionalSecret(name string) Secret { return Secret(e.Optional(name, "")) }

// URL returns the value of a required variable as a base URL. The URL must use
// http or https, must have a host and must have no user information, no query
// and no fragment. The error never shows the value, because a URL can carry a
// password.
func (e *Env) URL(name string) *url.URL {
	raw := e.Require(name)
	if raw == "" {
		return nil
	}
	u, err := ParseBaseURL(raw)
	if err != nil {
		e.fail("environment variable %s: %v", name, err)
		return nil
	}
	return u
}

// OptionalURL is like URL, but it returns def when the variable is empty.
func (e *Env) OptionalURL(name, def string) *url.URL {
	if strings.TrimSpace(e.get(name)) == "" {
		u, err := ParseBaseURL(def)
		if err != nil {
			e.fail("default of %s: %v", name, err)
		}
		return u
	}
	return e.URL(name)
}

// Duration returns a variable as a duration, or def when it is empty.
func (e *Env) Duration(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(e.get(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		e.fail("environment variable %s must be a positive duration such as 20s", name)
		return def
	}
	return d
}

// Int returns a variable as an integer in [lo, hi], or def when it is empty.
func (e *Env) Int(name string, def, lo, hi int) int {
	raw := strings.TrimSpace(e.get(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		e.fail("environment variable %s must be an integer from %d to %d", name, lo, hi)
		return def
	}
	return n
}

// ParseBaseURL checks a base URL of a vendor API.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("must use http or https")
	case u.Hostname() == "":
		return nil, errors.New("must have a host")
	case u.User != nil:
		return nil, errors.New("must not hold user information")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("must not hold a query or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}
