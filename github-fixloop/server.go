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

// The tools follow the GitHub REST API, version 2022-11-28:
//   - get a pull request, list pull requests, create a pull request, merge a
//     pull request with the sha parameter:
//     https://docs.github.com/en/rest/pulls/pulls
//   - the check runs of a commit:
//     https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference
//   - the combined commit status:
//     https://docs.github.com/en/rest/commits/statuses#get-the-combined-status-for-a-specific-reference
//   - git data: get a commit, get a reference, create a commit, create a reference:
//     https://docs.github.com/en/rest/git/commits and https://docs.github.com/en/rest/git/refs
//   - list the deployments of a repository, and the statuses of a deployment:
//     https://docs.github.com/en/rest/deployments/deployments and
//     https://docs.github.com/en/rest/deployments/statuses
//   - compare two commits:
//     https://docs.github.com/en/rest/commits/commits#compare-two-commits
//
// The input and the result of each tool follow internal/engine/fixloop/connector.go
// of zeroroot-ai/gibson. The mapping of the GitHub fields to the fields of the daemon:
//   - state: open is "open". closed with merged true is "merged". Other closed is "closed".
//     "refused" is an open pull request whose checks passed and whose mergeable_state
//     shows a rule that blocks the merge now. The tool reads it from GitHub on each call.
//   - pipeline_green: every check run is complete with the conclusion success, neutral or
//     skipped, and the combined status is success or has no status. At least one must exist.
//   - at: merged_at, closed_at or updated_at of the pull request.
//   - deployment_state.application: owner/repo. restart is always off.

const (
	maxRefLen    = 512
	maxCheckPage = 10 // pages of 100 check runs that one pipeline read takes
	maxDeploys   = 50
	maxAncestors = 20
	apiVersion   = "2022-11-28"
)

// mergeableWait is the time that the tool waits before it reads a pull request
// again when GitHub has not computed its mergeable state. Tests set it to zero.
var mergeableWait = time.Second

var (
	ownerFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoFormat  = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,100}$`)
	shortRef    = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9_.\-]{1,100})#(\d{1,9})$`)
	envFormat   = regexp.MustCompile(`^[A-Za-z0-9_./\-]{1,255}$`)
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.OptionalURL("GITHUB_API_URL", "https://api.github.com")
	token := env.Secret("GITHUB_TOKEN")
	environment := env.Require("GITHUB_ENVIRONMENT")
	timeout := env.Duration("GITHUB_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if !envFormat.MatchString(environment) {
		return nil, errors.New("GITHUB_ENVIRONMENT has the wrong format")
	}
	bearer := kit.HeaderAuth("Authorization", "Bearer ", token)
	client := kit.NewClient(base, kit.ClientOptions{
		Auth: func(ctx context.Context, req *http.Request) error {
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", apiVersion)
			return bearer(ctx, req)
		},
		Logger:     log,
		Timeout:    timeout,
		HTTPClient: hc,
	})
	host := base.Host
	if strings.EqualFold(host, "api.github.com") {
		host = "github.com"
	}
	svc := &service{client: client, host: host, environment: environment, sleep: sleepContext}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	client      *kit.Client
	host        string // host of the GitHub web address
	environment string
	sleep       func(context.Context, time.Duration) error
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "merge_request_state",
		Description: "Read the state of one GitHub pull request: open, merged, closed or refused, the status of its checks, " +
			"the merge commit and the time of the merge. Read only. " +
			"The state is refused when the checks passed and a rule of the repository blocks the merge now: a review, a " +
			"conflict, a draft, a branch that is behind. The reason is \"GitHub does not allow the merge: \" and the " +
			"mergeable_state of GitHub. The tool reads this from GitHub on each call and keeps no memory of it.",
		Annotations: kit.ReadOnly("GitHub pull request state"),
	}, v.mergeRequestState)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "merge_merge_request",
		Description: "Merge one GitHub pull request. The tool merges only when every check of the head commit passed and GitHub " +
			"reports the pull request as mergeable. It sends the head sha that it checked, so GitHub refuses the merge when a push " +
			"changed the head after the check. A pull request that is already merged is not an error. " +
			"Undo: call revert_merge_request with the same merge_request.",
		Annotations: kit.Reversible("Merge GitHub pull request", true),
	}, v.mergeMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "revert_merge_request",
		Description: "Revert one merged GitHub pull request. GitHub has no revert call, so the tool builds the revert with the git data API: " +
			"a new commit that has the tree of the parent of the merge commit, on a new branch, and a pull request from that branch. " +
			"It works only while the base branch still ends at the merge commit. It refuses a rebase merge of more than one " +
			"commit, because a revert of the merge commit would undo only part of the pull request. " +
			"It merges the revert pull request when its checks passed. Otherwise it returns the state of the revert pull request, " +
			"and the caller calls the tool again later. The same call twice opens one revert pull request. " +
			"When a person closed the revert pull request without a merge, the next call opens a new one on a new branch. " +
			"After "+strconv.Itoa(fixloop.RevertTries)+" closed revert pull requests, the tool returns an error, "+
			"and a person reverts by hand. " +
			"Undo: call revert_merge_request with the revert_merge_request value of the result.",
		Annotations: kit.Reversible("Revert GitHub pull request", true),
	}, v.revertMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "deployment_state",
		Description: "Read whether the merge commit of one merged GitHub pull request is deployed to the environment of this " +
			"server. A deployment of a later commit counts when it holds the merge commit. Read only. " +
			"The application is owner/repo. GitHub has no restart, so restart.needed is always false.",
		Annotations: kit.ReadOnly("GitHub deployment state"),
	}, v.deploymentState)
}

// ---- pull request model ----

type ghPR struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	HTMLURL        string `json:"html_url"`
	MergedAt       string `json:"merged_at"`
	ClosedAt       string `json:"closed_at"`
	UpdatedAt      string `json:"updated_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Commits        int    `json:"commits"`
	Head           struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

type prRef struct {
	owner, repo string
	number      int
}

func (r prRef) key() string { return r.owner + "/" + r.repo + "#" + strconv.Itoa(r.number) }

// parseRef reads the repository and the number from the URL of a pull request
// on this host, or from its short reference owner/repo#12.
func (v *service) parseRef(raw string) (prRef, error) {
	if err := kit.CheckString("merge_request", raw, maxRefLen, nil); err != nil {
		return prRef{}, err
	}
	bad := fmt.Errorf("%w: merge_request must be a pull request URL of the GitHub server, or owner/repo#12", kit.ErrInvalidInput)
	if m := shortRef.FindStringSubmatch(raw); len(m) == 4 && validRepo(m[2]) {
		n, _ := strconv.Atoi(m[3])
		return prRef{m[1], m[2], n}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Host, v.host) ||
		u.RawQuery != "" || u.User != nil {
		return prRef{}, bad
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" || !ownerFormat.MatchString(parts[0]) || !validRepo(parts[1]) {
		return prRef{}, bad
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n < 1 || len(parts[3]) > 9 {
		return prRef{}, bad
	}
	return prRef{parts[0], parts[1], n}, nil
}

func validRepo(r string) bool { return repoFormat.MatchString(r) && r != "." && r != ".." }

func repoPath(r prRef, rest ...string) string {
	return kit.Path(append([]string{"repos", r.owner, r.repo}, rest...)...)
}

func (v *service) getPR(ctx context.Context, r prRef) (*ghPR, error) {
	var pr ghPR
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "pulls", strconv.Itoa(r.number))}, &pr); err != nil {
		return nil, fmt.Errorf("read pull request: %w", err)
	}
	return &pr, nil
}

type checkRuns struct {
	Total int `json:"total_count"`
	Runs  []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

// pipeline reads the checks of a commit: the check runs and the combined
// commit status. The status is success only when every one passed.
func (v *service) pipeline(ctx context.Context, r prRef, sha string) (string, error) {
	failed, pending, seen, total := false, false, 0, 0
	for page := 1; page <= maxCheckPage; page++ {
		var cr checkRuns
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: repoPath(r, "commits", sha, "check-runs"),
			Query: url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}},
		}, &cr)
		if err != nil {
			return "", fmt.Errorf("read check runs: %w", err)
		}
		for _, run := range cr.Runs {
			seen++
			switch {
			case run.Status != "completed":
				pending = true
			case run.Conclusion != "success" && run.Conclusion != "neutral" && run.Conclusion != "skipped":
				failed = true
			}
		}
		total = cr.Total
		if len(cr.Runs) == 0 || seen >= cr.Total {
			break
		}
	}
	if seen < total {
		// A run that the tool did not read can be red. The commit is not green.
		return "", fmt.Errorf("the commit has %d check runs, and the tool reads at most %d of them. "+
			"It does not count the checks as passed", total, maxCheckPage*100)
	}
	var cs struct {
		State string `json:"state"`
		Total int    `json:"total_count"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "commits", sha, "status")}, &cs); err != nil {
		return "", fmt.Errorf("read commit status: %w", err)
	}
	if cs.Total > 0 {
		seen += cs.Total
		switch cs.State {
		case "failure", "error":
			failed = true
		case "pending":
			pending = true
		}
	}
	switch {
	case failed:
		return fixloop.PipelineFailed, nil
	case pending:
		return fixloop.PipelinePending, nil
	case seen == 0:
		return fixloop.PipelineNone, nil
	}
	return fixloop.PipelineSuccess, nil
}

// stateOf maps a GitHub pull request to the state of the daemon.
func (v *service) stateOf(ctx context.Context, r prRef, pr *ghPR) (fixloop.State, error) {
	st := fixloop.State{MergeRequest: r.key(), HeadSHA: pr.Head.SHA}
	status, err := v.pipeline(ctx, r, pr.Head.SHA)
	if err != nil {
		return st, err
	}
	st.PipelineStatus, st.PipelineGreen = status, status == fixloop.PipelineSuccess
	switch {
	case pr.State == "closed" && pr.Merged:
		st.State = fixloop.Merged
		st.MergeCommit = pr.MergeCommitSHA
		st.MergedAt = fixloop.Time(fixloop.ParseTime(pr.MergedAt))
		st.At = st.MergedAt
	case pr.State == "closed":
		st.State = fixloop.Closed
		st.At = fixloop.Time(fixloop.ParseTime(pr.ClosedAt))
	default:
		st.State = fixloop.Open
		st.At = fixloop.Time(fixloop.ParseTime(pr.UpdatedAt))
		if reason, ok := blocked(pr); ok && st.PipelineGreen {
			st.State, st.Reason = fixloop.Refused, reason
		}
	}
	return st, nil
}

// blocked returns the reason when the mergeable state of GitHub shows a rule
// of the repository that blocks the merge: a review, a conflict, a draft, a
// branch that is behind. The fix then waits on a person. A state that GitHub
// has not settled does not block.
func blocked(pr *ghPR) (string, bool) {
	switch pr.MergeableState {
	case "clean", "has_hooks", "unknown", "unstable", "":
		return "", false
	}
	return "GitHub does not allow the merge: " + pr.MergeableState, true
}

// ---- merge_request_state ----

func (v *service) mergeRequestState(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.State, error) {
	r, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, fixloop.State{}, err
	}
	pr, err := v.getPR(ctx, r)
	if err != nil {
		return nil, fixloop.State{}, err
	}
	st, err := v.stateOf(ctx, r, pr)
	return nil, st, err
}

// ---- merge_merge_request ----

type mergeInput struct {
	MergeRequest string `json:"merge_request" jsonschema:"URL of the pull request, or its reference owner/repo#12"`
	SHA          string `json:"sha,omitempty" jsonschema:"the head sha that the checks tested. The tool refuses the merge when the head is another commit."`
	MergeMethod  string `json:"merge_method,omitempty" jsonschema:"merge or squash. The default is merge. The revert tool does not support a rebase merge."`
}

func (v *service) mergeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in mergeInput) (*mcp.CallToolResult, fixloop.MergeResult, error) {
	r, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, fixloop.MergeResult{}, err
	}
	if in.SHA != "" {
		if err := kit.CheckString("sha", in.SHA, 40, kit.Hex40); err != nil {
			return nil, fixloop.MergeResult{}, err
		}
	}
	switch in.MergeMethod {
	case "", "merge", "squash":
	default:
		return nil, fixloop.MergeResult{}, fmt.Errorf("%w: merge_method must be merge or squash", kit.ErrInvalidInput)
	}
	pr, err := v.getPR(ctx, r)
	if err != nil {
		return nil, fixloop.MergeResult{}, err
	}
	st, merged, err := v.tryMerge(ctx, r, pr, in.SHA, in.MergeMethod)
	return nil, fixloop.NewMergeResult(st, merged), err
}

// tryMerge merges the pull request when its checks passed and GitHub reports
// it as mergeable. It sends the head sha that it checked, so a push after the
// check cannot be merged.
func (v *service) tryMerge(ctx context.Context, r prRef, pr *ghPR, wantSHA, method string) (fixloop.State, bool, error) {
	st, err := v.stateOf(ctx, r, pr)
	if err != nil {
		return st, false, err
	}
	switch {
	case st.State == fixloop.Merged:
		return st, true, nil
	case pr.State == "closed":
		st.Reason = "the pull request is closed"
		return st, false, nil
	case st.State == fixloop.Refused:
		return st, false, nil
	case wantSHA != "" && !strings.EqualFold(wantSHA, pr.Head.SHA):
		st.Reason = "the head of the pull request is not the sha that was tested. A push changed it."
		return st, false, nil
	case !st.PipelineGreen:
		st.Reason = "the checks have not succeeded: " + st.PipelineStatus
		return st, false, nil
	}
	if pr.Mergeable == nil || pr.MergeableState == "unknown" { // GitHub computes it on the first read
		if err := v.sleep(ctx, mergeableWait); err != nil {
			return st, false, err
		}
		if pr, err = v.getPR(ctx, r); err != nil {
			return st, false, err
		}
		if !strings.EqualFold(pr.Head.SHA, st.HeadSHA) {
			// The checks passed on st.HeadSHA only. A merge of the new head
			// would merge code that no check tested.
			st.Reason = "the head of the pull request changed after the check. A push changed it."
			return st, false, nil
		}
	}
	if reason, ok := blocked(pr); ok {
		st.State, st.Reason = fixloop.Refused, reason
		return st, false, nil
	}
	if pr.MergeableState != "clean" && pr.MergeableState != "has_hooks" {
		st.Reason = "GitHub has not settled the merge status: " + pr.MergeableState
		return st, false, nil
	}
	body := map[string]any{"sha": st.HeadSHA}
	if method != "" {
		body["merge_method"] = method
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPut, Path: repoPath(r, "pulls", strconv.Itoa(r.number), "merge"), JSON: body,
	}, nil)
	if err != nil {
		return v.mergeError(ctx, r, st, err)
	}
	latest, err := v.getPR(ctx, r)
	if err != nil {
		return st, false, err
	}
	st, err = v.stateOf(ctx, r, latest)
	return st, st.State == fixloop.Merged, err
}

// mergeError turns an error of the merge call into a result. A retry of a
// merge that GitHub already did answers with a refusal, so the tool reads the
// pull request again before it reports one.
func (v *service) mergeError(ctx context.Context, r prRef, st fixloop.State, err error) (fixloop.State, bool, error) {
	var ae *kit.APIError
	if !errors.As(err, &ae) {
		return st, false, fmt.Errorf("merge pull request: %w", err)
	}
	switch ae.Status {
	case http.StatusConflict:
		st.Reason = "GitHub refused the merge because the head changed after the check"
		return st, false, nil
	case http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		latest, rerr := v.getPR(ctx, r)
		if rerr == nil && latest.Merged {
			merged, serr := v.stateOf(ctx, r, latest)
			return merged, serr == nil, serr
		}
		st.State, st.Reason = fixloop.Refused, "GitHub refused the merge: "+briefBody(ae)
		return st, false, nil
	}
	return st, false, fmt.Errorf("merge pull request: %w", err)
}

// briefBody returns the message of a GitHub error body, or the HTTP status.
func briefBody(ae *kit.APIError) string {
	var b struct {
		Message string `json:"message"`
	}
	if kit.DecodeErrorBody(ae, &b) == nil && b.Message != "" {
		return b.Message
	}
	return fmt.Sprintf("HTTP %d", ae.Status)
}

// ---- revert_merge_request ----

type gitCommit struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

type gitRef struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

func refPath(r prRef, branch string) string {
	return repoPath(r, append([]string{"git", "ref", "heads"}, strings.Split(branch, "/")...)...)
}

func (v *service) revertMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.RevertResult, error) {
	out := fixloop.RevertResult{}
	r, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	out.MergeRequest = r.key()
	orig, err := v.getPR(ctx, r)
	if err != nil {
		return nil, out, err
	}
	if !orig.Merged || orig.MergeCommitSHA == "" {
		return nil, out, fmt.Errorf("%w: the pull request is not merged, so there is nothing to revert", kit.ErrInvalidInput)
	}
	branch := "zeroroot/revert-pr-" + strconv.Itoa(r.number) + "-" + orig.MergeCommitSHA[:8]
	revert, err := v.findOrOpenRevert(ctx, r, orig, branch)
	if err != nil {
		return nil, out, err
	}
	rr := prRef{r.owner, r.repo, revert.Number}
	revert, err = v.getPR(ctx, rr)
	if err != nil {
		return nil, out, err
	}
	st, merged, err := v.tryMerge(ctx, rr, revert, "", "")
	out.RevertMergeRequest = revert.HTMLURL
	out.State, out.PipelineGreen, out.PipelineStatus = st.State, st.PipelineGreen, st.PipelineStatus
	out.Merged, out.MergeCommit, out.Reason = merged, st.MergeCommit, st.Reason
	return nil, out, err
}

// findOrOpenRevert returns the revert pull request of the merge. The branch
// name holds the pull request and the commit, so a second call finds the work
// of the first call and does not repeat it. When a person closed the revert
// pull request without a merge, the next try opens a new one on a new branch.
func (v *service) findOrOpenRevert(ctx context.Context, r prRef, orig *ghPR, base string) (*ghPR, error) {
	for try := 1; try <= fixloop.RevertTries; try++ {
		branch := fixloop.RevertBranch(base, try)
		pr, err := v.findPR(ctx, r, branch)
		if err != nil {
			return nil, err
		}
		if pr == nil {
			return v.openRevert(ctx, r, orig, branch)
		}
		// The list of pull requests has merged_at, not merged.
		if pr.State != "closed" || pr.MergedAt != "" {
			return pr, nil
		}
	}
	return nil, fixloop.ErrRevertsClosed
}

// openRevert makes the revert commit and the branch, and opens the revert pull
// request from the branch.
func (v *service) openRevert(ctx context.Context, r prRef, orig *ghPR, branch string) (*ghPR, error) {
	commit := orig.MergeCommitSHA
	// The revert is exact only when no commit came after the merge: the tree of
	// the parent of the merge commit then equals the tree before the merge.
	var tip gitRef
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: refPath(r, orig.Base.Ref)}, &tip); err != nil {
		return nil, fmt.Errorf("read the base branch: %w", err)
	}
	var branchTip gitRef
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: refPath(r, branch)}, &branchTip)
	var ae *kit.APIError
	switch {
	case err == nil: // an earlier call made the branch and failed before the pull request
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		if tip.Object.SHA != commit {
			return nil, errors.New("the base branch has commits after the merge commit. A revert that uses the tree of the " +
				"parent would drop them, so revert by hand")
		}
		if err := v.createRevertBranch(ctx, r, orig, commit, branch); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("read the revert branch: %w", err)
	}
	var created ghPR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "pulls"),
		JSON: map[string]any{
			"title": fmt.Sprintf("Revert %q", orig.Title), "head": branch, "base": orig.Base.Ref,
			"body": "This pull request reverts the merge of " + orig.HTMLURL + " (commit " + commit + ").",
		},
	}, &created)
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		// A pull request from the branch exists already, for example from a
		// parallel call.
		if pr, ferr := v.findPR(ctx, r, branch); ferr == nil && pr != nil {
			return pr, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open the revert pull request: %w", err)
	}
	return &created, nil
}

func (v *service) findPR(ctx context.Context, r prRef, branch string) (*ghPR, error) {
	var found []ghPR
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "pulls"),
		Query: url.Values{"head": {r.owner + ":" + branch}, "state": {"all"}, "per_page": {"1"}},
	}, &found)
	if err != nil {
		return nil, fmt.Errorf("look for a revert pull request: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// createRevertBranch makes the revert commit with the git data API and the
// branch that points to it.
func (v *service) createRevertBranch(ctx context.Context, r prRef, orig *ghPR, commit, branch string) error {
	var merge, parent gitCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", commit)}, &merge); err != nil {
		return fmt.Errorf("read the merge commit: %w", err)
	}
	if len(merge.Parents) == 0 {
		return errors.New("the merge commit has no parent")
	}
	if len(merge.Parents) == 1 && orig.Commits > 1 {
		if err := v.checkSquash(ctx, r, orig, commit); err != nil {
			return err
		}
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", merge.Parents[0].SHA)}, &parent); err != nil {
		return fmt.Errorf("read the parent of the merge commit: %w", err)
	}
	var made gitCommit
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "git", "commits"),
		JSON: map[string]any{
			"message": fmt.Sprintf("Revert %q\n\nThis reverts commit %s.", orig.Title, commit),
			"tree":    parent.Tree.SHA, "parents": []string{commit},
		},
	}, &made)
	if err != nil {
		return fmt.Errorf("create the revert commit: %w", err)
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "git", "refs"),
		JSON: map[string]any{"ref": "refs/heads/" + branch, "sha": made.SHA},
	}, nil)
	if err != nil {
		return fmt.Errorf("create the revert branch: %w", err)
	}
	return nil
}

// checkSquash refuses a revert when the merge can be a rebase merge. A merge
// commit with one parent comes from a squash merge or a rebase merge. After a
// rebase merge, the merge commit is the last of the rebased commits, and the
// tree of its parent still holds the other commits of the pull request. A
// squash merge puts exactly one commit on the base, so the tool accepts the
// merge only when the base of the pull request is one commit behind the merge
// commit. A squash merge after the base moved on is refused too: the tool
// cannot prove that it is not a rebase merge.
func (v *service) checkSquash(ctx context.Context, r prRef, orig *ghPR, commit string) error {
	refuse := fmt.Errorf("the merge commit has one parent and the pull request has %d commits. This is a rebase merge, "+
		"or a squash merge after the base branch moved on. A revert of the merge commit can undo only part of the "+
		"pull request, so revert by hand", orig.Commits)
	if orig.Base.SHA == "" {
		return refuse
	}
	var cmp struct {
		Status  string `json:"status"`
		AheadBy int    `json:"ahead_by"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "compare", orig.Base.SHA+"..."+commit),
		Query: url.Values{"per_page": {"1"}},
	}, &cmp)
	if err != nil {
		return fmt.Errorf("compare the base with the merge commit: %w", err)
	}
	if cmp.Status != "ahead" || cmp.AheadBy != 1 {
		return refuse
	}
	return nil
}

// ---- deployment_state ----

type ghDeployment struct {
	ID        int    `json:"id"`
	SHA       string `json:"sha"`
	CreatedAt string `json:"created_at"`
}

type ghDeploymentStatus struct {
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

func (v *service) deploymentState(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.Deployment, error) {
	out := fixloop.Deployment{Environment: v.environment}
	r, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	out.Application = r.owner + "/" + r.repo
	pr, err := v.getPR(ctx, r)
	if err != nil {
		return nil, out, err
	}
	if !pr.Merged || pr.MergeCommitSHA == "" {
		out.Reason = "the pull request is not merged"
		return nil, out, nil
	}
	list, err := v.listDeployments(ctx, r, url.Values{"environment": {v.environment}, "sha": {pr.MergeCommitSHA}})
	if err != nil {
		return nil, out, err
	}
	// A deployment of a later commit holds the merge commit. The oldest one
	// that does is the deployment of the merge commit.
	later, err := v.listDeployments(ctx, r, url.Values{"environment": {v.environment}})
	if err != nil {
		return nil, out, err
	}
	merged := fixloop.ParseTime(pr.MergedAt)
	checks := 0
	for i := len(later) - 1; i >= 0; i-- {
		d := later[i]
		if t := fixloop.ParseTime(d.CreatedAt); !merged.IsZero() && t.Before(merged) {
			continue
		}
		if d.SHA != pr.MergeCommitSHA {
			if checks >= maxAncestors {
				break
			}
			checks++
			if !v.holds(ctx, r, pr.MergeCommitSHA, d.SHA) {
				continue
			}
		}
		list = append(list, d)
	}
	for _, d := range list {
		at, ok, err := v.succeeded(ctx, r, d.ID)
		if err != nil {
			return nil, out, err
		}
		if ok {
			out.Deployed = true
			out.Deployment = out.Application + "/deployments/" + strconv.Itoa(d.ID)
			out.At = fixloop.Time(at)
			return nil, out, nil
		}
	}
	out.Reason = "no successful deployment holds the merge commit yet"
	return nil, out, nil
}

func (v *service) listDeployments(ctx context.Context, r prRef, q url.Values) ([]ghDeployment, error) {
	q.Set("per_page", strconv.Itoa(maxDeploys))
	var list []ghDeployment
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "deployments"), Query: q}, &list); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	return list, nil
}

// succeeded returns the time of the first success status of a deployment. A
// deployment whose newest status is a failure or an error did not succeed.
func (v *service) succeeded(ctx context.Context, r prRef, id int) (time.Time, bool, error) {
	var sts []ghDeploymentStatus
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "deployments", strconv.Itoa(id), "statuses"),
		Query: url.Values{"per_page": {"30"}},
	}, &sts)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read deployment statuses: %w", err)
	}
	if len(sts) == 0 || sts[0].State == "failure" || sts[0].State == "error" {
		return time.Time{}, false, nil
	}
	var at time.Time
	for _, s := range sts { // newest first, so the last success is the first one in time
		if s.State == "success" {
			at = fixloop.ParseTime(s.CreatedAt)
		}
	}
	return at, !at.IsZero(), nil
}

// holds is true when the commit is an ancestor of the deployed commit: the
// deployed commit is identical to it or ahead of it.
func (v *service) holds(ctx context.Context, r prRef, commit, deployed string) bool {
	var cmp struct {
		Status string `json:"status"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "compare", commit+"..."+deployed),
		Query: url.Values{"per_page": {"1"}},
	}, &cmp)
	return err == nil && (cmp.Status == "ahead" || cmp.Status == "identical")
}
