// Package mockforge is a stateful mock of the GitHub REST API and of the
// Prometheus HTTP API. It stands in for the forge and the metrics of a buyer in
// the exit test of the fix loop (gibson#1469), so the github-fixloop and
// prometheus servers talk to it with no change. The tests of these servers use
// the same package.
//
// The mock keeps its state in memory. A test seeds and reads it through the
// control API under /_mock/. No real API uses that prefix.
//
// This is test code. It checks no credential and it must never run in a
// production install.
package mockforge

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Check is one check run of a commit.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// AutoDeploy makes a deployment of each commit that lands on the default
// branch. It stands in for the delivery pipeline of the buyer.
type AutoDeploy struct {
	Environment string `json:"environment"`
	State       string `json:"state"`
}

// Failure is an answer that the mock gives to each merge call of a repository.
type Failure struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type pull struct {
	Number     int
	Title      string
	Body       string
	State      string
	Merged     bool
	HeadRef    string
	HeadSHA    string
	BaseRef    string
	BaseSHA    string
	CreatedAt  string
	UpdatedAt  string
	ClosedAt   string
	MergedAt   string
	MergeSHA   string
	frozenList []string
}

type deployment struct {
	ID          int
	SHA         string
	Environment string
	CreatedAt   string
	Statuses    []depStatus // newest first
}

type depStatus struct {
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// Mock is the mock server. It is an http.Handler.
type Mock struct {
	mu    sync.Mutex
	now   func() time.Time
	repos map[string]*repo
	prom  promState
	// advisories are the security advisories that a test seeded. They apply to
	// each repository.
	advisories []Advisory
}

// New returns an empty mock. A nil now means time.Now.
func New(now func() time.Time) *Mock {
	if now == nil {
		now = time.Now
	}
	m := &Mock{now: now}
	m.reset()
	return m
}

func (m *Mock) reset() {
	m.repos = map[string]*repo{}
	m.advisories = nil
	m.prom = promState{health: map[string][]promPoint{}}
}

// ServeHTTP routes a request. One lock covers a request, so each call sees a
// whole state.
func (m *Mock) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := req.URL.EscapedPath()
	switch {
	case path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case strings.HasPrefix(path, "/_mock/"):
		m.control(w, req, splitPath(strings.TrimPrefix(path, "/_mock/")))
	case strings.HasPrefix(path, "/api/v1/"):
		m.prometheus(w, req, strings.TrimPrefix(path, "/api/v1/"))
	default:
		// A GitHub Enterprise Server serves the API under /api/v3.
		m.github(w, req, splitPath(strings.TrimPrefix(strings.TrimPrefix(path, "/api/v3"), "/")))
	}
}

// splitPath splits an escaped path and unescapes each segment, so a branch
// name with an escaped slash stays one segment.
func splitPath(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
		out = append(out, s)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func notFound(w http.ResponseWriter) { apiError(w, http.StatusNotFound, "Not Found") }

func readBody(w http.ResponseWriter, req *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<20))
	if err := dec.Decode(into); err != nil {
		apiError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	return true
}

func page(req *http.Request, n int) (from, to int) {
	per, _ := strconv.Atoi(req.URL.Query().Get("per_page"))
	if per < 1 || per > 100 {
		per = 30
	}
	pg, _ := strconv.Atoi(req.URL.Query().Get("page"))
	if pg < 1 {
		pg = 1
	}
	from = min((pg-1)*per, n)
	to = min(from+per, n)
	return from, to
}

func (m *Mock) repo(owner, name string) *repo {
	return m.repos[strings.ToLower(owner+"/"+name)]
}
