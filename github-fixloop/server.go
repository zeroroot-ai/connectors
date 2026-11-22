package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
//   - the combined commit status, 100 statuses on a page:
//     https://docs.github.com/en/rest/commits/statuses#get-the-combined-status-for-a-specific-reference
//   - git data: get a commit, get a reference, create a commit, create a reference:
//     https://docs.github.com/en/rest/git/commits and https://docs.github.com/en/rest/git/refs
//   - list the deployments of a repository, and the statuses of a deployment:
//     https://docs.github.com/en/rest/deployments/deployments and
//     https://docs.github.com/en/rest/deployments/statuses
//   - compare two commits:
//     https://docs.github.com/en/rest/commits/commits#compare-two-commits
//   - list the commits of a pull request (at most 250, 100 on a page):
//     https://docs.github.com/en/rest/pulls/pulls#list-commits-on-a-pull-request
//   - list the pull requests of a commit:
//     https://docs.github.com/en/rest/commits/commits#list-pull-requests-associated-with-a-commit
//   - the merge methods that a repository allows (allow_merge_commit,
//     allow_squash_merge, allow_rebase_merge). The page says: "To view
//     merge-related settings, you must have the contents:read and
//     contents:write permissions":
//     https://docs.github.com/en/rest/repos/repos#get-a-repository
//   - the checks that a rule of the base branch requires: branch protection in
//     "get a branch", and rulesets in "get rules for a branch":
//     https://docs.github.com/en/rest/branches/branches#get-a-branch and
//     https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch
//
// The input and the result of each tool follow internal/engine/fixloop/connector.go
// of zeroroot-ai/gibson. The mapping of the GitHub fields to the fields of the daemon:
//   - state: open is "open". closed with merged true is "merged". Other closed is "closed".
//     "refused" is an open pull request whose checks passed and whose mergeable_state
//     shows a rule that blocks the merge now, or a repository that allows no merge
//     method that the revert tool can undo. The tool reads it from GitHub on each call.
//   - pipeline_green: every check run is complete with the conclusion success, neutral or
//     skipped, and the combined status is success or has no status. At least one must exist.
//     For an open pull request, each check that a rule of the base branch requires must
//     exist. A required check that has not started makes the checks pending, and the
//     reason names each such check.
//   - at: merged_at, closed_at or updated_at of the pull request.
//   - source_branch and created_at: head.ref and created_at of the pull request.
//     source_branch is empty when head.repo.full_name is not base.repo.full_name,
//     or when head.repo is null (a deleted fork). source_branch_reason then says why.
//   - deployment_state.application: owner/repo. restart is always off.

const (
	maxRefLen    = 512
	maxCheckPage = 10 // pages of 100 check runs that one pipeline read takes
	maxDeploys   = 50
	maxAncestors = 20
	maxPRCommits = 250 // GitHub lists at most 250 commits of a pull request
	maxRulePages = 10  // pages of 100 rules that one read of the branch rules takes
	maxStatPages = 10  // pages of 100 statuses that one read of the combined status takes
	apiVersion   = "2022-11-28"
)

// The reasons of a refusal by the merge methods of the repository. They do
// not change between calls, so the daemon can tell one refusal from another.
const (
	reasonNoMethod = "GitHub does not allow the merge: the repository allows neither a merge commit nor a squash merge, " +
		"and the revert tool cannot undo a rebase merge"
	reasonMethodOff = "GitHub does not allow the merge: the repository does not allow the merge method "
	// reasonWaiting starts the reason of pending checks when a check that a
	// rule of the base branch requires has not reported. The names follow.
	reasonWaiting = "these required checks have not reported: "
)

// errHiddenMethods is the error when GitHub does not show the merge methods
// of the repository. It is not a refusal: the repository did not block the
// merge, the token cannot read the settings.
var errHiddenMethods = errors.New("GitHub does not show the merge methods of the repository (allow_merge_commit, " +
	"allow_squash_merge). The token needs the contents:read and contents:write permissions of the repository")

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
	v.registerOpen(s, log)
	v.registerReadFile(s, log)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "merge_request_state",
		Description: "Read the state of one GitHub pull request: open, merged, closed or refused, the status of its checks, " +
			"the merge commit and the time of the merge. Read only. " +
			"A check that a rule of the base branch requires and that has not started makes the checks pending. " +
			"The state is refused when the checks passed and a rule of the repository blocks the merge now: a review, a " +
			"conflict, a draft, a branch that is behind, or a repository that allows neither a merge commit nor a squash " +
			"merge. The reason starts with \"GitHub does not allow the merge: \". The tool reads this from GitHub on each " +
			"call and keeps no memory of it.",
		Annotations: kit.ReadOnly("GitHub pull request state"),
	}, v.mergeRequestState)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "merge_merge_request",
		Description: "Merge one GitHub pull request. The tool merges only when every check of the head commit passed and GitHub " +
			"reports the pull request as mergeable. It sends the head sha that it checked, so GitHub refuses the merge when a push " +
			"changed the head after the check. It uses a merge method that the repository allows: merge, then squash. " +
			"A pull request that is already merged is not an error. " +
			"Undo: call revert_merge_request with the same merge_request.",
		Annotations: kit.Reversible("Merge GitHub pull request", true),
	}, v.mergeMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "revert_merge_request",
		Description: "Revert one merged GitHub pull request. GitHub has no revert call, so the tool builds the revert with the git data API: " +
			"a new commit that has the tree of the parent of the merge commit, on a new branch, and a pull request from that branch. " +
			"It works only while the base branch still ends at the merge commit. It refuses a rebase merge of more than one " +
			"commit, because a revert of the merge commit would undo only part of the pull request. " +
			"It refuses a revert branch that a person made, when its commit is not the revert commit of the tool. " +
			"It merges the revert pull request when its checks passed. Otherwise it returns the state of the revert pull request, " +
			"and the caller calls the tool again later. The same call twice opens one revert pull request. " +
			"When a person closed the revert pull request without a merge, the next call opens a new one on a new branch. " +
			"After " + strconv.Itoa(fixloop.RevertTries) + " closed revert pull requests, the tool returns an error, " +
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
	CreatedAt      string `json:"created_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Commits        int    `json:"commits"`
	Head           struct {
		SHA  string  `json:"sha"`
		Ref  string  `json:"ref"`
		Repo *ghRepo `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string  `json:"ref"`
		SHA  string  `json:"sha"`
		Repo *ghRepo `json:"repo"`
	} `json:"base"`
}

// ghRepo is the repository of the head or the base of a pull request. GitHub
// gives a null head repository when a person deleted the fork.
type ghRepo struct {
	FullName string `json:"full_name"`
}

// fromFork is true when the head of the pull request is not in the base
// repository. A deleted fork has no head repository, so it counts as a fork.
func (pr *ghPR) fromFork() bool {
	h, b := pr.Head.Repo, pr.Base.Repo
	return h == nil || b == nil || h.FullName == "" || !strings.EqualFold(h.FullName, b.FullName)
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
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

// runResult sums up the check runs of a commit.
type runResult struct {
	names           map[string]bool
	failed, pending bool
	seen            int
}

// pipeline reads the checks of a commit: the check runs and the combined
// commit status. The status is success only when every one passed. A name in
// required that no check run and no status has makes the status pending: GitHub
// blocks the merge until that check reports. The second result holds the names
// of these required checks.
func (v *service) pipeline(
	ctx context.Context, r prRef, sha string, required []string,
) (status string, waiting []string, err error) {
	rr, err := v.checkRunsOf(ctx, r, sha)
	if err != nil {
		return "", nil, err
	}
	cs, err := v.combinedStatus(ctx, r, sha)
	if err != nil {
		return "", nil, err
	}
	// A required check that has not reported can still fail.
	waiting = missing(required, rr.names, cs)
	failed, pending, seen := rr.failed, rr.pending || len(waiting) > 0, rr.seen+cs.Total
	if cs.Total > 0 {
		switch cs.State {
		case "failure", "error":
			failed = true
		case "pending":
			pending = true
		}
	}
	switch {
	case failed:
		return fixloop.PipelineFailed, waiting, nil
	case pending:
		return fixloop.PipelinePending, waiting, nil
	case seen == 0:
		return fixloop.PipelineNone, waiting, nil
	}
	return fixloop.PipelineSuccess, waiting, nil
}

// checkRunsOf reads each page of the check runs of a commit. It returns an
// error when the commit has more runs than it reads.
func (v *service) checkRunsOf(ctx context.Context, r prRef, sha string) (runResult, error) {
	rr := runResult{names: map[string]bool{}}
	total := 0
	for page := 1; page <= maxCheckPage; page++ {
		var cr checkRuns
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: repoPath(r, "commits", sha, "check-runs"),
			Query: url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}},
		}, &cr)
		if err != nil {
			return rr, fmt.Errorf("read check runs: %w", err)
		}
		for _, run := range cr.Runs {
			rr.seen++
			rr.names[run.Name] = true
			switch {
			case run.Status != "completed":
				rr.pending = true
			case run.Conclusion != "success" && run.Conclusion != "neutral" && run.Conclusion != "skipped":
				rr.failed = true
			}
		}
		total = cr.Total
		if len(cr.Runs) == 0 || rr.seen >= cr.Total {
			break
		}
	}
	if rr.seen < total {
		// A run that the tool did not read can be red. The commit is not green.
		return rr, fmt.Errorf("the commit has %d check runs, and the tool reads at most %d of them. "+
			"It does not count the checks as passed", total, maxCheckPage*100)
	}
	return rr, nil
}

type combined struct {
	State    string `json:"state"`
	Total    int    `json:"total_count"`
	Statuses []struct {
		Context string `json:"context"`
	} `json:"statuses"`
}

// combinedStatus reads the combined commit status and each page of its
// statuses. It returns an error when the commit has more statuses than it
// reads, because a required check can be in a status that it did not read.
func (v *service) combinedStatus(ctx context.Context, r prRef, sha string) (*combined, error) {
	var all combined
	for page := 1; page <= maxStatPages; page++ {
		var cs combined
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: repoPath(r, "commits", sha, "status"),
			Query: url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}},
		}, &cs)
		if err != nil {
			return nil, fmt.Errorf("read commit status: %w", err)
		}
		if page == 1 {
			all.State, all.Total = cs.State, cs.Total
		}
		all.Statuses = append(all.Statuses, cs.Statuses...)
		if len(cs.Statuses) < 100 || len(all.Statuses) >= all.Total {
			break
		}
	}
	if len(all.Statuses) < all.Total {
		return nil, fmt.Errorf("the commit has %d statuses, and the tool read %d of them. "+
			"It does not count the checks as passed", all.Total, len(all.Statuses))
	}
	return &all, nil
}

// missing returns each name in required that has no check run in runs and no
// status in cs, once, in the order of required.
func missing(required []string, runs map[string]bool, cs *combined) []string {
	seen := maps.Clone(runs)
	for _, s := range cs.Statuses {
		seen[s.Context] = true
	}
	var out []string
	for _, name := range required {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true // list a name once
		}
	}
	return out
}

// stateOf maps a GitHub pull request to the state of the daemon.
func (v *service) stateOf(ctx context.Context, r prRef, pr *ghPR) (fixloop.State, error) {
	st := fixloop.State{
		MergeRequest: r.key(), HeadSHA: pr.Head.SHA,
		SourceBranch: pr.Head.Ref, CreatedAt: fixloop.Time(fixloop.ParseTime(pr.CreatedAt)),
	}
	if pr.fromFork() {
		st.SourceBranch, st.SourceBranchReason = "", fixloop.ForkReason
	}
	var required []string
	if pr.State != "closed" {
		var err error
		if required, err = v.requiredChecks(ctx, r, pr.Base.Ref); err != nil {
			return st, err
		}
	}
	status, waiting, err := v.pipeline(ctx, r, pr.Head.SHA, required)
	if err != nil {
		return st, err
	}
	st.PipelineStatus, st.PipelineGreen = status, status == fixloop.PipelineSuccess
	if len(waiting) > 0 {
		st.Reason = reasonWaiting + strings.Join(waiting, ", ")
	}
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
		if !st.PipelineGreen {
			// The checks are not complete. A block now can come from a check
			// that has not reported, so it is not a refusal yet.
			break
		}
		if reason, ok := blocked(pr); ok {
			st.State, st.Reason = fixloop.Refused, reason
			break
		}
		_, refusal, err := v.mergeMethod(ctx, r, "")
		if err != nil {
			return st, err
		}
		if refusal != "" {
			st.State, st.Reason = fixloop.Refused, refusal
		}
	}
	return st, nil
}

// requiredChecks returns the names of the checks that a rule of the branch
// requires: the branch protection, and the rules of the rulesets. GitHub
// Enterprise Server before rulesets answers 404 for the rules, which means no
// rules.
func (v *service) requiredChecks(ctx context.Context, r prRef, branch string) ([]string, error) {
	segs := strings.Split(branch, "/")
	var b struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, append([]string{"branches"}, segs...)...)}, &b); err != nil {
		return nil, fmt.Errorf("read the base branch protection: %w", err)
	}
	names := append([]string{}, b.Protection.RequiredStatusChecks.Contexts...)
	for _, c := range b.Protection.RequiredStatusChecks.Checks {
		names = append(names, c.Context)
	}
	for page := 1; page <= maxRulePages; page++ {
		var rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: repoPath(r, append([]string{"rules", "branches"}, segs...)...),
			Query: url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}},
		}, &rules)
		var ae *kit.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the base branch rules: %w", err)
		}
		for _, rule := range rules {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, c := range rule.Parameters.RequiredStatusChecks {
				names = append(names, c.Context)
			}
		}
		if len(rules) < 100 {
			return names, nil
		}
	}
	return names, nil
}

// mergeMethod returns the merge method for the merge call: want when the
// repository allows it, or else merge, then squash. It never returns rebase,
// because the revert tool cannot undo a rebase merge of more than one commit.
// A refusal is a stable reason, and the state tool reports the same one. A
// token that cannot read the merge methods gets errHiddenMethods.
func (v *service) mergeMethod(ctx context.Context, r prRef, want string) (method, refusal string, err error) {
	var repo struct {
		AllowMerge  *bool `json:"allow_merge_commit"`
		AllowSquash *bool `json:"allow_squash_merge"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r)}, &repo); err != nil {
		return "", "", fmt.Errorf("read the merge methods of the repository: %w", err)
	}
	if repo.AllowMerge == nil || repo.AllowSquash == nil {
		return "", "", errHiddenMethods
	}
	allowed := map[string]bool{"merge": *repo.AllowMerge, "squash": *repo.AllowSquash}
	if want != "" {
		if !allowed[want] {
			return "", reasonMethodOff + want, nil
		}
		return want, "", nil
	}
	for _, m := range []string{"merge", "squash"} {
		if allowed[m] {
			return m, "", nil
		}
	}
	return "", reasonNoMethod, nil
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
	if err != nil {
		return nil, st, err
	}
	st.ChangedFiles = []fixloop.ChangedFile{}
	files, truncated, ferr := v.changedFiles(ctx, r)
	if ferr != nil {
		st.ChangedFilesError = ferr.Error()
	} else {
		st.ChangedFiles, st.ChangedFilesTruncated = files, truncated
	}
	return nil, st, nil
}

// ---- merge_merge_request ----

type mergeInput struct {
	MergeRequest string `json:"merge_request" jsonschema:"URL of the pull request, or its reference owner/repo#12"`
	SHA          string `json:"expected_head_sha,omitempty" jsonschema:"the head sha that the checks tested, from head_sha of merge_request_state or open_merge_request. The tool refuses the merge when the head is another commit, and sends this sha to GitHub."`
	MergeMethod  string `json:"merge_method,omitempty" jsonschema:"merge or squash. The default is the first that the repository allows: merge, then squash. The revert tool does not support a rebase merge."`
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
	if st.State == fixloop.Merged {
		return st, true, nil
	}
	if pr.fromFork() {
		st.State, st.Reason = fixloop.Refused, fixloop.ForkReason
		return st, false, nil
	}
	if holdMerge(&st, pr, wantSHA) {
		return st, false, nil
	}
	pr, ok, err := v.settle(ctx, r, pr, &st)
	if err != nil || !ok {
		return st, false, err
	}
	if reason, ok := blocked(pr); ok {
		st.State, st.Reason = fixloop.Refused, reason
		return st, false, nil
	}
	if pr.MergeableState != "clean" && pr.MergeableState != "has_hooks" {
		st.Reason = "GitHub has not settled the merge status: " + pr.MergeableState
		return st, false, nil
	}
	method, refusal, err := v.mergeMethod(ctx, r, method)
	if err != nil {
		return st, false, err
	}
	if refusal != "" {
		st.State, st.Reason = fixloop.Refused, refusal
		return st, false, nil
	}
	body := map[string]any{"sha": st.HeadSHA, "merge_method": method}
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

// holdMerge is true when the state shows that the tool must not merge now. It
// sets the reason. A reason of the state, such as the names of the required
// checks that have not reported, stays in the reason.
func holdMerge(st *fixloop.State, pr *ghPR, wantSHA string) bool {
	switch {
	case pr.State == "closed":
		st.Reason = "the pull request is closed"
	case wantSHA != "" && !strings.EqualFold(wantSHA, pr.Head.SHA):
		st.Reason = "the head of the pull request moved: expected " + wantSHA + ", now " + pr.Head.SHA + ". A push changed it."
	case st.State == fixloop.Refused:
	case !st.PipelineGreen:
		reason := "the checks have not succeeded: " + st.PipelineStatus
		if st.Reason != "" {
			reason += ". " + st.Reason
		}
		st.Reason = reason
	default:
		return false
	}
	return true
}

// settle reads the pull request again when GitHub has not computed its
// mergeable state, which GitHub does on the first read. It is false, with the
// reason in st, when the head changed in the meantime.
func (v *service) settle(ctx context.Context, r prRef, pr *ghPR, st *fixloop.State) (*ghPR, bool, error) {
	if pr.Mergeable != nil && pr.MergeableState != "unknown" {
		return pr, true, nil
	}
	if err := v.sleep(ctx, mergeableWait); err != nil {
		return pr, false, err
	}
	latest, err := v.getPR(ctx, r)
	if err != nil {
		return pr, false, err
	}
	if !strings.EqualFold(latest.Head.SHA, st.HeadSHA) {
		// The checks passed on st.HeadSHA only. A merge of the new head
		// would merge code that no check tested.
		st.Reason = "the head of the pull request changed after the check. A push changed it."
		return latest, false, nil
	}
	return latest, true, nil
}

// mergeError turns an error of the merge call into a result. A retry of a
// merge that GitHub already did answers with a refusal, so the tool reads the
// pull request again before it reports one. A refusal fails closed: the state
// is refused, and the reason holds the HTTP status and the message of GitHub,
// which do not change while the cause stays.
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
		st.State, st.Reason = fixloop.Refused, fmt.Sprintf("GitHub refused the merge (HTTP %d): %s", ae.Status, briefBody(ae))
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
	return "no message"
}

// ---- revert_merge_request ----

type gitCommit struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Author  gitPerson `json:"author"`
	Tree    struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// gitPerson is the author of a commit. A rebase keeps the author, its date
// and the message of a commit, and gives the commit a new sha.
type gitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
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
	if orig.fromFork() {
		out.State, out.Reason = fixloop.Refused, fixloop.ForkReason
		return nil, out, nil
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
	case err == nil:
		// An earlier call made the branch and failed before the pull request,
		// or a person made a branch with this name. Keep only the first.
		if err := v.checkRevertBranch(ctx, r, orig, commit, branch, branchTip.Object.SHA); err != nil {
			return nil, err
		}
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		if tip.Object.SHA != commit {
			return nil, errors.New("the base branch has commits after the merge commit. A revert that uses the tree of the " +
				"parent would drop them, so revert by hand")
		}
		tree, err := v.revertTree(ctx, r, orig, commit)
		if err != nil {
			return nil, err
		}
		if err := v.createRevertBranch(ctx, r, orig, commit, tree, branch); err != nil {
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

// revertTree returns the tree of the revert commit: the tree of the first
// parent of the merge commit. It refuses a rebase merge of more than one commit.
func (v *service) revertTree(ctx context.Context, r prRef, orig *ghPR, commit string) (string, error) {
	var merge, parent gitCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", commit)}, &merge); err != nil {
		return "", fmt.Errorf("read the merge commit: %w", err)
	}
	if len(merge.Parents) == 0 {
		return "", errors.New("the merge commit has no parent")
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", merge.Parents[0].SHA)}, &parent); err != nil {
		return "", fmt.Errorf("read the parent of the merge commit: %w", err)
	}
	if len(merge.Parents) == 1 && orig.Commits > 1 {
		if err := v.checkSquash(ctx, r, orig, &parent); err != nil {
			return "", err
		}
	}
	return parent.Tree.SHA, nil
}

// checkRevertBranch accepts a revert branch that exists already only when its
// tip is the revert commit that this tool makes: one parent, which is the
// merge commit, and the tree of the parent of the merge commit.
func (v *service) checkRevertBranch(ctx context.Context, r prRef, orig *ghPR, commit, branch, tipSHA string) error {
	var tip gitCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", tipSHA)}, &tip); err != nil {
		return fmt.Errorf("read the commit of the revert branch: %w", err)
	}
	tree, err := v.revertTree(ctx, r, orig, commit)
	if err != nil {
		return err
	}
	if len(tip.Parents) != 1 || tip.Parents[0].SHA != commit || tip.Tree.SHA != tree {
		return fmt.Errorf("the branch %s exists, and its last commit is not the revert commit of this tool. "+
			"A person made the branch. Revert by hand, or delete the branch", branch)
	}
	return nil
}

// createRevertBranch makes the revert commit with the git data API and the
// branch that points to it.
func (v *service) createRevertBranch(ctx context.Context, r prRef, orig *ghPR, commit, tree, branch string) error {
	var made gitCommit
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "git", "commits"),
		JSON: map[string]any{
			"message": fmt.Sprintf("Revert %q\n\nThis reverts commit %s.", orig.Title, commit),
			"tree":    tree, "parents": []string{commit},
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

// checkSquash refuses a revert of a rebase merge. A merge commit with one
// parent comes from a squash merge or a rebase merge
// (https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/about-merge-methods-on-github):
//   - After a squash merge, the parent is the tip of the base branch before
//     the merge. That commit is never a commit of the pull request.
//   - After a rebase merge of n commits, the merge commit is the last rebased
//     commit, and its parent is the rebased commit n-1 of the pull request. A
//     revert to the tree of that parent undoes only the last commit.
//
// A rebase merge gives each commit a new sha, so the sha alone does not show a
// rebased commit. A rebase keeps the author, the author date and the message.
// So the tool refuses when the parent has the sha, or the author, the author
// date and the message, of a commit of the pull request. A commit of the base
// branch that has all of these of a commit of the pull request is refused too.
// That refusal is safe: a person then reverts by hand.
//
// No test on a live GitHub has confirmed that a rebase merge keeps the author
// date. So a second check that does not use it follows: parentInPull.
func (v *service) checkSquash(ctx context.Context, r prRef, orig *ghPR, parent *gitCommit) error {
	if orig.Commits > maxPRCommits {
		return fmt.Errorf("the merge commit has one parent and the pull request has %d commits. GitHub lists at most "+
			"%d commits of a pull request, so the tool cannot tell a squash merge from a rebase merge. Revert by hand",
			orig.Commits, maxPRCommits)
	}
	for page := 1; page <= (orig.Commits+99)/100; page++ {
		var list []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string    `json:"message"`
				Author  gitPerson `json:"author"`
			} `json:"commit"`
		}
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: repoPath(r, "pulls", strconv.Itoa(r.number), "commits"),
			Query: url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}},
		}, &list)
		if err != nil {
			return fmt.Errorf("list the commits of the pull request: %w", err)
		}
		for _, c := range list {
			if c.SHA == parent.SHA || sameChange(c.Commit.Author, c.Commit.Message, parent) {
				return rebaseError(orig.Commits)
			}
		}
		if len(list) < 100 {
			break
		}
	}
	return v.parentInPull(ctx, r, orig, parent.SHA)
}

// parentInPull refuses the revert when GitHub lists the pull request as a pull
// request of the parent of the merge commit. The page
// https://docs.github.com/en/rest/commits/commits#list-pull-requests-associated-with-a-commit
// says: "Lists the merged pull request that introduced the commit to the
// repository." For a commit that is not in the default branch, it lists the
// merged and open pull requests of the commit.
//   - After a rebase merge, the parent is a rebased commit of the pull request.
//     The pull request introduced it, so GitHub lists the pull request.
//   - After a squash merge, the parent is the tip of the base branch before the
//     merge. Another pull request or a push introduced it.
//
// A failed call, or a full page that does not hold the pull request, refuses
// the revert, because the tool then cannot tell the two merges apart.
func (v *service) parentInPull(ctx context.Context, r prRef, orig *ghPR, parent string) error {
	var list []struct {
		Number int `json:"number"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "commits", parent, "pulls"),
		Query: url.Values{"per_page": {"100"}},
	}, &list)
	if err != nil {
		return fmt.Errorf("list the pull requests of the parent of the merge commit. The tool cannot tell a squash "+
			"merge from a rebase merge, so revert by hand: %w", err)
	}
	for _, p := range list {
		if p.Number == r.number {
			return rebaseError(orig.Commits)
		}
	}
	if len(list) >= 100 {
		return errors.New("the parent of the merge commit has 100 pull requests or more, so the tool cannot tell " +
			"a squash merge from a rebase merge. Revert by hand")
	}
	return nil
}

// rebaseError is the refusal of a revert of a rebase merge of n commits.
func rebaseError(n int) error {
	return fmt.Errorf("the merge commit has one parent, and that parent is a commit of the pull request. "+
		"This is a rebase merge of %d commits. A revert of the merge commit undoes only the last commit, "+
		"so revert by hand", n)
}

// sameChange is true when a commit has the author, the author date and the
// message of another commit: a rebase made one from the other.
func sameChange(author gitPerson, message string, c *gitCommit) bool {
	at, ct := fixloop.ParseTime(author.Date), fixloop.ParseTime(c.Author.Date)
	return author.Name == c.Author.Name && author.Email == c.Author.Email && !at.IsZero() && at.Equal(ct) &&
		strings.TrimSpace(message) == strings.TrimSpace(c.Message)
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
