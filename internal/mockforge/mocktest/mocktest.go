// Package mocktest starts the mock forge for a test and gives short calls to
// its control API.
package mocktest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/mockforge"
)

// Server is a running mock. It closes at the end of the test.
type Server struct {
	*httptest.Server
	t testing.TB
}

// Start runs a mock on a local port.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{Server: httptest.NewServer(mockforge.New(nil)), t: t}
	t.Cleanup(s.Close)
	return s
}

// Do sends one request with an optional JSON body. It returns the status and the
// decoded JSON object of the answer. The answer can be empty.
func (s *Server) Do(method, path string, body any) (int, map[string]any) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("encode body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.URL+path, rd)
	if err != nil {
		s.t.Fatalf("make request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// Post sends a POST to the control API and fails the test on an error status.
func (s *Server) Post(path string, body any) map[string]any {
	s.t.Helper()
	status, out := s.Do(http.MethodPost, path, body)
	if status >= 300 {
		s.t.Fatalf("POST %s: HTTP %d: %v", path, status, out)
	}
	return out
}

// Get reads one object and fails the test on an error status.
func (s *Server) Get(path string) map[string]any {
	s.t.Helper()
	status, out := s.Do(http.MethodGet, path, nil)
	if status >= 300 {
		s.t.Fatalf("GET %s: HTTP %d: %v", path, status, out)
	}
	return out
}

// Seed makes a repository on the branch main with files. A value is a string
// or an object with content and mode. It returns the head commit of main.
func (s *Server) Seed(owner, repo string, files map[string]any, more map[string]any) string {
	s.t.Helper()
	body := map[string]any{"owner": owner, "repo": repo, "files": files}
	for k, v := range more {
		body[k] = v
	}
	out := s.Post("/_mock/repos", body)
	sha, _ := out["head_sha"].(string)
	return sha
}
