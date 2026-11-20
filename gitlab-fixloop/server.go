package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
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
//
// The input and the result of each tool follow internal/engine/fixloop/connector.go
// of zeroroot-ai/gibson. The mapping of the GitLab fields to the fields of the daemon:
//   - state: opened and locked are "open". merged is "merged". closed is "closed".
//     "refused" is an open merge request whose head pipeline succeeded and whose
//     detailed_merge_status shows a rule that blocks the merge now. The tool reads it
//     from GitLab on each call.
//   - pipeline_green: true when head_pipeline.status is "success" and head_pipeline.sha is the head sha.
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
			"The state is refused when the head pipeline succeeded and a rule of the project blocks the merge now: an " +
			"approval, a thread, a draft, a conflict. The reason is \"GitLab does not allow the merge: \" and the " +
			"detailed_merge_status of GitLab. The tool reads this from GitLab on each call and keeps no memory of it.",
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
			"one on a new branch. After "+strconv.Itoa(fixloop.RevertTries)+" closed revert merge requests, the tool returns " +
			"an error, and a person reverts by hand. " +
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
}

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
func stateOf(key string, mr *glMR) fixloop.State {
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
	case "closed":
		st.State = fixloop.Closed
		st.At = fixloop.Time(fixloop.ParseTime(mr.ClosedAt))
	default:
		st.State = fixloop.Open
		st.At = fixloop.Time(fixloop.ParseTime(mr.UpdatedAt))
		if status := mergeStatus(mr); green && status != "mergeable" && !transientStatus[status] {
			// A rule of the project blocks the merge: an approval, a thread, a
			// draft, a conflict. The fix waits on a person.
			st.State, st.Reason = fixloop.Refused, "GitLab does not allow the merge: "+status
		}
	}
	return st
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
	return nil, stateOf(refKey(project, iid), mr), nil
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
var transientStatus = map[string]bool{"checking": true, "unchecked": true, "preparing": true, "approvals_syncing": true,
	"ci_still_running": true, "ci_must_pass": true}

// tryMerge merges the merge request when its head pipeline succeeded and
// GitLab reports it as mergeable. It sends the head sha of its check, so a push
// after the check cannot be merged.
func (v *service) tryMerge(ctx context.Context, project string, iid int, mr *glMR, wantSHA string) (fixloop.State, bool, error) {
	key := refKey(project, iid)
	st := stateOf(key, mr)
	switch {
	case mr.State == "merged":
		return st, true, nil
	case mr.State == "closed":
		st.Reason = "the merge request is closed"
		return st, false, nil
	case st.State == fixloop.Refused:
		return st, false, nil
	case wantSHA != "" && !strings.EqualFold(wantSHA, mr.SHA):
		st.Reason = "the head of the merge request is not the sha that was tested. A push changed it."
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
	err := v.client.Do(ctx, kit.Request{
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
		return stateOf(key, &merged), merged.State == "merged", nil
	}
	return v.mergeError(ctx, project, iid, st, err)
}

// mergeError turns an error of the merge call into a result. A retry of a
// merge that GitLab already did answers with a refusal, so the tool reads the
// merge request again before it reports one.
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
			return stateOf(key, latest), true, nil
		}
		st.State, st.Reason = fixloop.Refused, "GitLab refused the merge: "+briefBody(ae)
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
	return fmt.Sprintf("HTTP %d", ae.Status)
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
// revert commit on it. A branch that holds the revert commit already is kept.
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
		if len(cmp.Commits) > 0 {
			return nil // an earlier call did the revert commit
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
