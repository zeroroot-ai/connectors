package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the GitLab REST API v4:
//   - merge requests (get, list, create, merge with the sha parameter):
//     https://docs.gitlab.com/api/merge_requests/
//   - the detailed_merge_status values:
//     https://docs.gitlab.com/api/merge_requests/#merge-status
//   - revert a commit onto a branch:
//     https://docs.gitlab.com/api/commits/#revert-a-commit
//   - create a branch, and compare two refs:
//     https://docs.gitlab.com/api/branches/ and https://docs.gitlab.com/api/repositories/
//   - the merge base of two commits:
//     https://docs.gitlab.com/api/repositories/#merge-base
//   - list the deployments of a project:
//     https://docs.gitlab.com/api/deployments/
//   - the merge method of a project (merge, rebase_merge or ff):
//     https://docs.gitlab.com/api/projects/
//   - the external status checks of a merge request (passed, pending, failed):
//     https://docs.gitlab.com/api/status_checks/
//   - get one commit (parent_ids):
//     https://docs.gitlab.com/api/commits/#get-a-single-commit
//
// The input and the result of each tool follow internal/engine/fixloop/connector.go
// of zeroroot-ai/gibson. The mapping of the GitLab fields to the fields of the daemon:
//   - state: opened and locked are "open". merged is "merged". closed is "closed".
//     "refused" is an open merge request whose head pipeline succeeded and whose
//     detailed_merge_status shows a rule that blocks the merge now, or a project
//     that merges by fast-forward with no squash. The tool reads it from GitLab on
//     each call.
//   - pipeline_green: true when head_pipeline.status is "success" and head_pipeline.sha is the head sha.
//     When detailed_merge_status is status_checks_must_pass, the external status checks
//     count too: a failed one makes the pipeline "failed", a pending one "pending".
//   - at: merged_at, closed_at or updated_at of the merge request.
//   - deployment_state.application: the path of the project. restart is always off.

const (
	maxRefLen    = 512
	maxDeploys   = 50 // deployments that one deployment_state call reads
	maxAncestors = 20 // merge base calls that one deployment_state call makes
)

var (
	projectFormat = regexp.MustCompile(`^[A-Za-z0-9_.\-]+(/[A-Za-z0-9_.\-]+)*$`)
	shortRef      = regexp.MustCompile(`^([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)*)!(\d{1,9})$`)
	numberPrefix  = regexp.MustCompile(`^(\d{1,9})(?:/.*)?$`)
	envFormat     = regexp.MustCompile(`^[A-Za-z0-9_./\-]{1,255}$`)
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("GITLAB_BASE_URL")
	token := env.Secret("GITLAB_TOKEN")
	environment := env.Require("GITLAB_ENVIRONMENT")
	timeout := env.Duration("GITLAB_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if !envFormat.MatchString(environment) {
		return nil, errors.New("GITLAB_ENVIRONMENT has the wrong format")
	}
	api := *base
	api.Path += "/api/v4"
	client := kit.NewClient(&api, kit.ClientOptions{
		Auth:       kit.HeaderAuth("PRIVATE-TOKEN", "", token),
		Logger:     log,
		Timeout:    timeout,
		HTTPClient: hc,
	})
	svc := &service{client: client, host: base.Host, environment: environment}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	client      *kit.Client
	host        string // host and port of the GitLab web address
	environment string
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "merge_request_state",
		Description: "Read the state of one GitLab merge request: open, merged, closed or refused, the status of its head " +
			"pipeline, the merge commit and the time of the merge. Read only. " +
			"An external status check that has not passed makes the pipeline pending or failed. " +
			"The state is refused when the head pipeline succeeded and a rule of the project blocks the merge now: an " +
			"approval, a thread, a draft, a conflict, or a fast-forward merge with no squash, which leaves no commit to " +
			"revert. The reason starts with \"GitLab does not allow the merge: \". The tool reads this from GitLab on " +
			"each call and keeps no memory of it.",
		Annotations: kit.ReadOnly("GitLab merge request state"),
	}, v.mergeRequestState)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "merge_merge_request",
		Description: "Merge one GitLab merge request. The tool merges only when the head pipeline succeeded and GitLab reports " +
			"the merge request as mergeable. It sends the head sha that it checked, so GitLab refuses the merge when a push " +
			"changed the head after the check. A merge request that is already merged is not an error. " +
			"Undo: call revert_merge_request with the same merge_request.",
		Annotations: kit.Reversible("Merge GitLab merge request", true),
	}, v.mergeMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "revert_merge_request",
		Description: "Revert one merged GitLab merge request. The tool opens a new merge request that reverts the merge commit, " +
			"and merges it when its head pipeline succeeded. When the pipeline has not finished, it returns the state of the " +
			"revert merge request. Call the tool again with the same input to merge it later. The same call twice opens one " +
			"revert merge request. When a person closed the revert merge request without a merge, the next call opens a new " +
			"one on a new branch. After " + strconv.Itoa(fixloop.RevertTries) + " closed revert merge requests, " +
			"the tool returns an error, and a person reverts by hand. " +
			"It refuses a revert branch that a person made, when its commit does not revert the merge commit. " +
			"Undo: call revert_merge_request with the revert_merge_request value of the result.",
		Annotations: kit.Reversible("Revert GitLab merge request", true),
	}, v.revertMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "deployment_state",
		Description: "Read whether the merge commit of one merged GitLab merge request is deployed to the environment of this " +
			"server. A deployment of a later commit counts when it holds the merge commit. Read only. " +
			"The application is the path of the project. GitLab has no restart, so restart.needed is always false.",
		Annotations: kit.ReadOnly("GitLab deployment state"),
	}, v.deploymentState)
}

// ---- merge request model ----

type glPipeline struct {
	ID     int    `json:"id"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
}

type glMR struct {
	IID                 int         `json:"iid"`
	Title               string      `json:"title"`
	State               string      `json:"state"`
	WebURL              string      `json:"web_url"`
	SHA                 string      `json:"sha"`
	MergeCommitSHA      string      `json:"merge_commit_sha"`
	SquashCommitSHA     string      `json:"squash_commit_sha"`
	MergedAt            string      `json:"merged_at"`
	ClosedAt            string      `json:"closed_at"`
	UpdatedAt           string      `json:"updated_at"`
	SourceBranch        string      `json:"source_branch"`
	TargetBranch        string      `json:"target_branch"`
	MergeStatus         string      `json:"merge_status"`
	DetailedMergeStatus string      `json:"detailed_merge_status"`
	HeadPipeline        *glPipeline `json:"head_pipeline"`
	Squash              bool        `json:"squash"`
	SquashOnMerge       *bool       `json:"squash_on_merge"`
}

// squashes is true when GitLab squashes the merge request on merge. GitLab
// recommends squash_on_merge, which holds the project settings. Older GitLab
// has squash only.
func (mr *glMR) squashes() bool {
	if mr.SquashOnMerge != nil {
		return *mr.SquashOnMerge
	}
	return mr.Squash
}

// reasonFastForward is the refusal of a project that merges by fast-forward
// without a squash. Such a merge leaves no merge commit and no squash commit,
// so revert_merge_request cannot undo it. The reason does not change between
// calls.
const reasonFastForward = "GitLab does not allow the merge: the project merges by fast-forward and this merge request " +
	"does not squash, so the merge leaves no commit that the revert tool can revert"

// parseRef reads the project path and the number from the URL of a merge
// request on this host, or from its short reference group/project!12.
func (v *service) parseRef(raw string) (project string, number int, err error) {
	if err := kit.CheckString("merge_request", raw, maxRefLen, nil); err != nil {
		return "", 0, err
	}
	bad := fmt.Errorf("%w: merge_request must be a merge request URL of the GitLab server, or group/project!12", kit.ErrInvalidInput)
	if m := shortRef.FindStringSubmatch(raw); len(m) == 3 && fixloop.ValidPath(m[1]) {
		n, _ := strconv.Atoi(m[2])
		return m[1], n, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Host, v.host) ||
		u.RawQuery != "" || u.User != nil {
		return "", 0, bad
	}
	if !fixloop.ValidPath(strings.Trim(u.Path, "/")) {
		return "", 0, bad
	}
	path := strings.Trim(u.Path, "/")
	for _, sep := range []string{"/-/merge_requests/", "/merge_requests/"} {
		if i := strings.Index(path, sep); i > 0 {
			m := numberPrefix.FindStringSubmatch(path[i+len(sep):])
			if m == nil || !projectFormat.MatchString(path[:i]) {
				return "", 0, bad
			}
			n, _ := strconv.Atoi(m[1])
			return path[:i], n, nil
		}
	}
	return "", 0, bad
}

func refKey(project string, iid int) string { return project + "!" + strconv.Itoa(iid) }

func mrPath(project string, iid int, rest ...string) string {
	return kit.Path(append([]string{"projects", project, "merge_requests", strconv.Itoa(iid)}, rest...)...)
}

func (v *service) getMR(ctx context.Context, project string, iid int) (*glMR, error) {
	var mr glMR
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: mrPath(project, iid)}, &mr); err != nil {
		return nil, fmt.Errorf("read merge request: %w", err)
	}
	return &mr, nil
}

func pipelineStatus(mr *glMR) (string, bool) {
	if mr.HeadPipeline == nil {
		return fixloop.PipelineNone, false
	}
	// A pipeline counts only when it ran on the head commit. An empty sha
	// proves nothing about the code that the merge would take.
	green := mr.HeadPipeline.Status == "success" && mr.SHA != "" && mr.HeadPipeline.SHA == mr.SHA
	return mr.HeadPipeline.Status, green
}

// stateOf maps a GitLab merge request to the state of the daemon.
func (v *service) stateOf(ctx context.Context, project, key string, mr *glMR) (fixloop.State, error) {
	status, green := pipelineStatus(mr)
	st := fixloop.State{MergeRequest: key, PipelineStatus: status, PipelineGreen: green, HeadSHA: mr.SHA}
	switch mr.State {
	case "merged":
		st.State = fixloop.Merged
		st.MergeCommit = mr.MergeCommitSHA
		if st.MergeCommit == "" {
			st.MergeCommit = mr.SquashCommitSHA
		}
		st.MergedAt = fixloop.Time(fixloop.ParseTime(mr.MergedAt))
		st.At = st.MergedAt
		return st, nil
	case "closed":
		st.State = fixloop.Closed
		st.At = fixloop.Time(fixloop.ParseTime(mr.ClosedAt))
		return st, nil
	}
	st.State = fixloop.Open
	st.At = fixloop.Time(fixloop.ParseTime(mr.UpdatedAt))
	status = mergeStatus(mr)
	if green && statusCheckStatus[status] {
		checks, err := v.statusChecks(ctx, project, mr.IID)
		if err != nil {
			return st, err
		}
		if checks != fixloop.PipelineSuccess {
			// An external status check is a check of the merge request. While
			// it runs or after it failed, the checks are not green.
			st.PipelineStatus, st.PipelineGreen = checks, false
			return st, nil
		}
	}
	if !st.PipelineGreen || status == "mergeable" || transientStatus[status] {
		if st.PipelineGreen {
			return v.fastForwardRefusal(ctx, project, mr, st)
		}
		return st, nil
	}
	// A rule of the project blocks the merge: an approval, a thread, a draft,
	// a conflict. The fix waits on a person.
	st.State, st.Reason = fixloop.Refused, "GitLab does not allow the merge: "+status
	return st, nil
}

// fastForwardRefusal marks the state refused when the project merges by
// fast-forward and the merge request does not squash.
func (v *service) fastForwardRefusal(ctx context.Context, project string, mr *glMR, st fixloop.State) (fixloop.State, error) {
	if mr.squashes() {
		return st, nil
	}
	var p struct {
		MergeMethod string `json:"merge_method"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", project)}, &p); err != nil {
		return st, fmt.Errorf("read the merge method of the project: %w", err)
	}
	if p.MergeMethod == "ff" {
		st.State, st.Reason = fixloop.Refused, reasonFastForward
	}
	return st, nil
}

// statusChecks reads the external status checks of a merge request. Any
// failed check is "failed". Else any check that has not passed is "pending".
func (v *service) statusChecks(ctx context.Context, project string, iid int) (string, error) {
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: mrPath(project, iid, "status_checks")}, &list); err != nil {
		return "", fmt.Errorf("read the external status checks: %w", err)
	}
	result := fixloop.PipelineSuccess
	for _, c := range list {
		switch c.Status {
		case "passed":
		case "failed":
			return fixloop.PipelineFailed, nil
		default:
			result = fixloop.PipelinePending
		}
	}
	return result, nil
}

// mergeStatus returns the detailed_merge_status of the merge request. GitLab
// before 15.6 has merge_status only.
func mergeStatus(mr *glMR) string {
	if mr.DetailedMergeStatus != "" {
		return mr.DetailedMergeStatus
	}
	if status := map[string]string{"can_be_merged": "mergeable", "cannot_be_merged": "conflict"}[mr.MergeStatus]; status != "" {
		return status
	}
	return "checking"
}

// ---- merge_request_state ----

func (v *service) mergeRequestState(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.State, error) {
	project, iid, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, fixloop.State{}, err
	}
	mr, err := v.getMR(ctx, project, iid)
	if err != nil {
		return nil, fixloop.State{}, err
	}
	st, err := v.stateOf(ctx, project, refKey(project, iid), mr)
	return nil, st, err
}

// ---- merge_merge_request ----

type mergeInput struct {
	MergeRequest string `json:"merge_request" jsonschema:"URL of the merge request, or its reference group/project!12"`
	SHA          string `json:"sha,omitempty" jsonschema:"the head sha that the pipeline tested. The tool refuses the merge when the head is another commit."`
}

func (v *service) mergeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in mergeInput) (*mcp.CallToolResult, fixloop.MergeResult, error) {
	project, iid, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, fixloop.MergeResult{}, err
	}
	if in.SHA != "" {
		if err := kit.CheckString("sha", in.SHA, 40, kit.Hex40); err != nil {
			return nil, fixloop.MergeResult{}, err
		}
	}
	mr, err := v.getMR(ctx, project, iid)
	if err != nil {
		return nil, fixloop.MergeResult{}, err
	}
	st, merged, err := v.tryMerge(ctx, project, iid, mr, in.SHA)
	return nil, fixloop.NewMergeResult(st, merged), err
}

// Classes of detailed_merge_status that are not "mergeable".
// https://docs.gitlab.com/api/merge_requests/#merge-status
// merge_time is "May not be merged until after the specified time". The time
// passes with no person, so it is not a refusal.
var transientStatus = map[string]bool{"checking": true, "unchecked": true, "preparing": true, "approvals_syncing": true,
	"ci_still_running": true, "ci_must_pass": true, "merge_time": true}

// statusCheckStatus holds the detailed_merge_status of external status checks
// that have not all passed. The page names it status_checks_must_pass. Some
// GitLab versions answer external_status_checks.
var statusCheckStatus = map[string]bool{"status_checks_must_pass": true, "external_status_checks": true}

// tryMerge merges the merge request when its head pipeline succeeded and
// GitLab reports it as mergeable. It sends the head sha of its check, so a push
// after the check cannot be merged.
func (v *service) tryMerge(ctx context.Context, project string, iid int, mr *glMR, wantSHA string) (fixloop.State, bool, error) {
	key := refKey(project, iid)
	st, err := v.stateOf(ctx, project, key, mr)
	if err != nil {
		return st, false, err
	}
	switch {
	case mr.State == "merged":
		return st, true, nil
	case mr.State == "closed":
		st.Reason = "the merge request is closed"
		return st, false, nil
	case wantSHA != "" && !strings.EqualFold(wantSHA, mr.SHA):
		st.Reason = "the head of the merge request is not the sha that was tested. A push changed it."
		return st, false, nil
	case st.State == fixloop.Refused:
		return st, false, nil
	case !st.PipelineGreen:
		st.Reason = "the head pipeline has not succeeded: " + st.PipelineStatus
		return st, false, nil
	}
	if status := mergeStatus(mr); status != "mergeable" {
		st.Reason = "GitLab has not settled the merge status: " + status
		return st, false, nil
	}
	var merged glMR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPut, Path: mrPath(project, iid, "merge"),
		JSON: map[string]any{"sha": mr.SHA},
	}, &merged)
	if err == nil {
		if merged.State != "merged" { // the answer is not settled: read it again
			latest, rerr := v.getMR(ctx, project, iid)
			if rerr != nil {
				return st, false, rerr
			}
			merged = *latest
		}
		st, err := v.stateOf(ctx, project, key, &merged)
		return st, merged.State == "merged", err
	}
	return v.mergeError(ctx, project, iid, st, err)
}

// mergeError turns an error of the merge call into a result. A retry of a
// merge that GitLab already did answers with a refusal, so the tool reads the
// merge request again before it reports one. A refusal fails closed: the state
// is refused, and the reason holds the HTTP status and the message of GitLab,
// which do not change while the cause stays.
func (v *service) mergeError(ctx context.Context, project string, iid int, st fixloop.State, err error) (fixloop.State, bool, error) {
	var ae *kit.APIError
	if !errors.As(err, &ae) {
		return st, false, fmt.Errorf("merge merge request: %w", err)
	}
	switch ae.Status {
	case http.StatusConflict:
		st.Reason = "GitLab refused the merge because the head changed after the check"
		return st, false, nil
	case http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusUnprocessableEntity:
		key := refKey(project, iid)
		latest, rerr := v.getMR(ctx, project, iid)
		if rerr == nil && latest.State == "merged" {
			merged, serr := v.stateOf(ctx, project, key, latest)
			return merged, serr == nil, serr
		}
		st.State, st.Reason = fixloop.Refused, fmt.Sprintf("GitLab refused the merge (HTTP %d): %s", ae.Status, briefBody(ae))
		return st, false, nil
	}
	return st, false, fmt.Errorf("merge merge request: %w", err)
}

// briefBody returns the message of a GitLab error body, or the raw start of it.
func briefBody(ae *kit.APIError) string {
	var b struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if kit.DecodeErrorBody(ae, &b) == nil {
		if m, ok := b.Message.(string); ok {
			return m
		}
		if b.Error != "" {
			return b.Error
		}
	}
	return "no message"
}

// ---- revert_merge_request ----

type apiBranchCompare struct {
	Commits []struct {
		ID string `json:"id"`
	} `json:"commits"`
}

func (v *service) revertMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.RevertResult, error) {
	out := fixloop.RevertResult{}
	project, iid, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	out.MergeRequest = refKey(project, iid)
	orig, err := v.getMR(ctx, project, iid)
	if err != nil {
		return nil, out, err
	}
	commit := orig.MergeCommitSHA
	if commit == "" {
		commit = orig.SquashCommitSHA
	}
	switch {
	case orig.State != "merged":
		return nil, out, fmt.Errorf("%w: the merge request is not merged, so there is nothing to revert", kit.ErrInvalidInput)
	case commit == "":
		return nil, out, errors.New("GitLab gives no merge commit or squash commit for this merge request. " +
			"A fast-forward merge has neither, so revert the commits by hand")
	}
	branch := "zeroroot/revert-mr" + strconv.Itoa(iid) + "-" + commit[:8]
	revertIID, err := v.findOrOpenRevert(ctx, project, orig, commit, branch)
	if err != nil {
		return nil, out, err
	}
	revert, err := v.getMR(ctx, project, revertIID)
	if err != nil {
		return nil, out, err
	}
	st, merged, err := v.tryMerge(ctx, project, revertIID, revert, "")
	out.RevertMergeRequest = revert.WebURL
	out.State, out.PipelineGreen, out.PipelineStatus = st.State, st.PipelineGreen, st.PipelineStatus
	out.Merged, out.MergeCommit, out.Reason = merged, st.MergeCommit, st.Reason
	return nil, out, err
}

// findOrOpenRevert returns the number of the revert merge request of the
// merge. The branch name holds the merge request and the commit, so a second
// call finds the work of the first call and does not repeat it. When a person
// closed the revert merge request without a merge, the next try opens a new
// one on a new branch.
func (v *service) findOrOpenRevert(ctx context.Context, project string, orig *glMR, commit, base string) (int, error) {
	for try := 1; try <= fixloop.RevertTries; try++ {
		branch := fixloop.RevertBranch(base, try)
		var found []glMR
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: kit.Path("projects", project, "merge_requests"),
			Query: url.Values{"source_branch": {branch}, "target_branch": {orig.TargetBranch}, "state": {"all"}, "per_page": {"1"}},
		}, &found)
		if err != nil {
			return 0, fmt.Errorf("look for a revert merge request: %w", err)
		}
		if len(found) == 0 {
			return v.openRevert(ctx, project, orig, commit, branch)
		}
		if found[0].State != "closed" {
			return found[0].IID, nil
		}
	}
	return 0, fixloop.ErrRevertsClosed
}

// openRevert puts the revert commit on the branch and opens the revert merge
// request from the branch.
func (v *service) openRevert(ctx context.Context, project string, orig *glMR, commit, branch string) (int, error) {
	if err := v.ensureRevertBranch(ctx, project, orig.TargetBranch, branch, commit); err != nil {
		return 0, err
	}
	var created glMR
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "merge_requests"),
		JSON: map[string]any{
			"source_branch": branch, "target_branch": orig.TargetBranch,
			"title":                fmt.Sprintf("Revert %q", orig.Title),
			"description":          "This merge request reverts the merge of " + orig.WebURL + " (commit " + commit + ").",
			"remove_source_branch": true,
		},
	}, &created)
	if err != nil {
		return 0, fmt.Errorf("open the revert merge request: %w", err)
	}
	return created.IID, nil
}

// ensureRevertBranch makes the branch from the target branch and puts the
// revert commit on it. A branch that exists already is kept only when its one
// commit after the target branch reverts the merge commit.
func (v *service) ensureRevertBranch(ctx context.Context, project, target, branch, commit string) error {
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "repository", "branches"),
		Query: url.Values{"branch": {branch}, "ref": {target}},
	}, nil)
	var ae *kit.APIError
	switch {
	case err == nil:
	case errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Body, "already exists"):
		var cmp apiBranchCompare
		cerr := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "compare"),
			Query: url.Values{"from": {target}, "to": {branch}},
		}, &cmp)
		if cerr != nil {
			return fmt.Errorf("compare the revert branch: %w", cerr)
		}
		switch len(cmp.Commits) {
		case 0: // an earlier call made the branch and failed before the revert
		case 1:
			// An earlier call did the revert commit, or a person made the branch.
			return v.checkRevertCommit(ctx, project, branch, commit, cmp.Commits[0].ID)
		default:
			return fmt.Errorf("the branch %s exists and has %d commits that the target branch does not have. "+
				"A person made the branch. Revert by hand, or delete the branch", branch, len(cmp.Commits))
		}
	default:
		return fmt.Errorf("create the revert branch: %w", err)
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "repository", "commits", commit, "revert"),
		JSON: map[string]any{"branch": branch},
	}, nil)
	if err != nil {
		return fmt.Errorf("revert the merge commit: %w", err)
	}
	return nil
}

type glCommit struct {
	ID        string   `json:"id"`
	ParentIDs []string `json:"parent_ids"`
}

func (v *service) getCommit(ctx context.Context, project, sha string) (*glCommit, error) {
	var c glCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "commits", sha)}, &c); err != nil {
		return nil, fmt.Errorf("read commit: %w", err)
	}
	return &c, nil
}

// checkRevertCommit accepts the commit of an existing revert branch only when
// it is the commit that the revert call of GitLab makes: one parent, which
// holds the merge commit, and a change that is the change of the merge commit
// back to its first parent. The tool compares the added and removed lines of
// each file. The line numbers and the context lines can differ, because the
// target branch can have moved on before the revert.
func (v *service) checkRevertCommit(ctx context.Context, project, branch, commit, revert string) error {
	refuse := fmt.Errorf("the branch %s exists, and its commit does not revert the merge commit %s. "+
		"A person made the branch. Revert by hand, or delete the branch", branch, commit)
	rc, err := v.getCommit(ctx, project, revert)
	if err != nil {
		return err
	}
	mc, err := v.getCommit(ctx, project, commit)
	if err != nil {
		return err
	}
	if len(rc.ParentIDs) != 1 || len(mc.ParentIDs) == 0 || !v.holds(ctx, project, commit, rc.ParentIDs[0]) {
		return refuse
	}
	want, err := v.change(ctx, project, commit, mc.ParentIDs[0])
	if err != nil {
		return err
	}
	got, err := v.change(ctx, project, rc.ParentIDs[0], revert)
	if err != nil {
		return err
	}
	if want == "" || want != got {
		return refuse
	}
	return nil
}

// change returns the change from one commit to another as text: for each file,
// the paths, the kind of change, and the added and removed lines. It returns
// an error when GitLab cuts the diff, because a cut diff proves nothing.
func (v *service) change(ctx context.Context, project, from, to string) (string, error) {
	var cmp struct {
		CompareTimeout bool `json:"compare_timeout"`
		Diffs          []struct {
			OldPath     string `json:"old_path"`
			NewPath     string `json:"new_path"`
			Diff        string `json:"diff"`
			NewFile     bool   `json:"new_file"`
			RenamedFile bool   `json:"renamed_file"`
			DeletedFile bool   `json:"deleted_file"`
			TooLarge    bool   `json:"too_large"`
			Collapsed   bool   `json:"collapsed"`
		} `json:"diffs"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "compare"),
		Query: url.Values{"from": {from}, "to": {to}, "straight": {"true"}},
	}, &cmp)
	if err != nil {
		return "", fmt.Errorf("compare two commits: %w", err)
	}
	if cmp.CompareTimeout {
		return "", errors.New("GitLab cut the compare of two commits, so the tool cannot check the revert branch. " +
			"Revert by hand, or delete the branch")
	}
	files := make([]string, 0, len(cmp.Diffs))
	for _, d := range cmp.Diffs {
		if d.TooLarge || d.Collapsed {
			return "", errors.New("GitLab cut a diff of two commits, so the tool cannot check the revert branch. " +
				"Revert by hand, or delete the branch")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s\x00%s\x00%t%t%t\n", d.OldPath, d.NewPath, d.NewFile, d.RenamedFile, d.DeletedFile)
		for _, line := range strings.Split(d.Diff, "\n") {
			if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
		files = append(files, b.String())
	}
	sort.Strings(files)
	return strings.Join(files, "\x01"), nil
}

// ---- deployment_state ----

type glDeployment struct {
	ID         int    `json:"id"`
	SHA        string `json:"sha"`
	Status     string `json:"status"`
	UpdatedAt  string `json:"updated_at"`
	FinishedAt string `json:"finished_at"`
}

func (v *service) deploymentState(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.Deployment, error) {
	out := fixloop.Deployment{Environment: v.environment}
	project, iid, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	out.Application = project
	mr, err := v.getMR(ctx, project, iid)
	if err != nil {
		return nil, out, err
	}
	commit := mr.MergeCommitSHA
	if commit == "" {
		commit = mr.SquashCommitSHA
	}
	if mr.State != "merged" || commit == "" {
		out.Reason = "the merge request is not merged, or GitLab gives no merge commit for it"
		return nil, out, nil
	}
	// GitLab answers 400 when updated_after comes without order_by=updated_at.
	q := url.Values{"environment": {v.environment}, "status": {"success"}, "order_by": {"updated_at"}, "sort": {"asc"},
		"per_page": {strconv.Itoa(maxDeploys)}}
	if t := fixloop.ParseTime(mr.MergedAt); !t.IsZero() {
		q.Set("updated_after", fixloop.Time(t))
	}
	var list []glDeployment
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", project, "deployments"), Query: q}, &list); err != nil {
		return nil, out, fmt.Errorf("list deployments: %w", err)
	}
	checks := 0
	for _, d := range list {
		if d.SHA != commit {
			if checks >= maxAncestors {
				break
			}
			checks++
			if !v.holds(ctx, project, commit, d.SHA) {
				continue
			}
		}
		out.Deployed = true
		out.Deployment = project + "/deployments/" + strconv.Itoa(d.ID)
		at := d.FinishedAt
		if at == "" {
			at = d.UpdatedAt
		}
		out.At = fixloop.Time(fixloop.ParseTime(at))
		return nil, out, nil
	}
	out.Reason = "no successful deployment holds the merge commit yet"
	return nil, out, nil
}

// holds is true when the commit is an ancestor of the deployed commit: the
// merge base of the two is the commit itself.
func (v *service) holds(ctx context.Context, project, commit, deployed string) bool {
	var base struct {
		ID string `json:"id"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "merge_base"),
		Query: url.Values{"refs[]": {commit, deployed}},
	}, &base)
	return err == nil && base.ID == commit
}
