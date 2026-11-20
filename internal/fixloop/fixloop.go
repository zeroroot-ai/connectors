// Package fixloop holds the parts that the Git host servers share for the fix
// loop of the Gibson daemon. The tool names, the arguments and the result
// fields follow internal/engine/fixloop/connector.go in zeroroot-ai/gibson.
package fixloop

import (
	"strings"
	"sync"
	"time"
)

// The states of a merge request. They match MROpen, MRMerged, MRClosed and
// MRRefused of the daemon.
const (
	Open    = "open"
	Merged  = "merged"
	Closed  = "closed"
	Refused = "refused"
)

// The values of PipelineStatus.
const (
	PipelineNone    = "none"
	PipelinePending = "pending"
	PipelineSuccess = "success"
	PipelineFailed  = "failed"
)

// MergeRequestInput is the argument of the tools that read, merge or revert
// one merge request. The value is the URL of the merge request, or its short
// reference. The daemon sends the key that the FIXED_BY link of the finding
// holds.
type MergeRequestInput struct {
	MergeRequest string `json:"merge_request" jsonschema:"URL of the merge request, or its reference: group/project!12 on GitLab, owner/repo#12 on GitHub"`
}

// State is the result of merge_request_state. The daemon reads state,
// pipeline_green, reason and at. The other fields are for the agent.
type State struct {
	MergeRequest   string `json:"merge_request"`
	State          string `json:"state"`
	PipelineGreen  bool   `json:"pipeline_green"`
	PipelineStatus string `json:"pipeline_status"`
	Reason         string `json:"reason,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	MergeCommit    string `json:"merge_commit,omitempty"`
	MergedAt       string `json:"merged_at,omitempty"`
	At             string `json:"at,omitempty"`
}

// MergeResult is the result of merge_merge_request.
type MergeResult struct {
	MergeRequest   string `json:"merge_request"`
	State          string `json:"state"`
	PipelineGreen  bool   `json:"pipeline_green"`
	PipelineStatus string `json:"pipeline_status"`
	Reason         string `json:"reason,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	MergeCommit    string `json:"merge_commit,omitempty"`
	MergedAt       string `json:"merged_at,omitempty"`
	At             string `json:"at,omitempty"`
	Merged         bool   `json:"merged"`
}

// NewMergeResult builds the result of merge_merge_request from a state.
func NewMergeResult(s State, merged bool) MergeResult {
	return MergeResult{
		MergeRequest: s.MergeRequest, State: s.State, PipelineGreen: s.PipelineGreen,
		PipelineStatus: s.PipelineStatus, Reason: s.Reason, HeadSHA: s.HeadSHA, MergeCommit: s.MergeCommit,
		MergedAt: s.MergedAt, At: s.At, Merged: merged,
	}
}

// ValidPath is false for a path that has an empty, "." or ".." segment.
func ValidPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// RevertResult is the result of revert_merge_request.
type RevertResult struct {
	MergeRequest       string `json:"merge_request"`
	RevertMergeRequest string `json:"revert_merge_request"`
	State              string `json:"state"`
	PipelineGreen      bool   `json:"pipeline_green"`
	PipelineStatus     string `json:"pipeline_status"`
	Merged             bool   `json:"merged"`
	MergeCommit        string `json:"merge_commit,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// Restart is the restart part of the deployment result. The Git hosts do not
// know about a restart, so Needed is always false.
type Restart struct {
	Needed  bool   `json:"needed"`
	Started string `json:"started,omitempty"`
	Ended   string `json:"ended,omitempty"`
}

// Deployment is the result of deployment_state.
type Deployment struct {
	Deployed    bool    `json:"deployed"`
	Deployment  string  `json:"deployment,omitempty"`
	Application string  `json:"application,omitempty"`
	At          string  `json:"at,omitempty"`
	Environment string  `json:"environment,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Restart     Restart `json:"restart"`
}

// Time formats a time as the daemon reads it: RFC 3339 in UTC. A zero time is
// an empty string.
func Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ParseTime reads a time from a Git host. It returns the zero time for an
// empty or a bad value.
func ParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Refusals remembers the merges that the host refused. A merge request has no
// "refused" state on a Git host, so the server records the refusal that it saw
// and reports it from merge_request_state while the head of the merge request
// stays the same. The memory is in the process. A restart clears it, and the
// next merge attempt records the refusal again.
type Refusals struct {
	mu sync.Mutex
	m  map[string]refusal
}

type refusal struct{ sha, reason string }

// Note records a refusal for the merge request at the head sha.
func (r *Refusals) Note(key, sha, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]refusal{}
	}
	r.m[key] = refusal{sha: sha, reason: reason}
}

// Get returns the refusal of the merge request at the head sha. A refusal of
// an older head does not count.
func (r *Refusals) Get(key, sha string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.m[key]
	if !ok || f.sha != sha {
		return "", false
	}
	return f.reason, true
}

// Clear forgets the refusal of the merge request.
func (r *Refusals) Clear(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key)
}
