package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// open_merge_request uses these GitLab REST API v4 calls:
//   - the files of a repository, to tell a new file from an existing one
//     (HEAD answers 200 or 404):  https://docs.gitlab.com/api/repository_files/
//   - create a commit with actions (create, update, delete) and start_branch,
//     which makes the new branch from the target branch in the same call:
//     https://docs.gitlab.com/api/commits/
//   - list and create merge requests: https://docs.gitlab.com/api/merge_requests/
//
// The token needs the scope api and the role Developer of the project.

func (v *service) registerOpen(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "open_merge_request",
		Description: "Open one GitLab merge request that holds a change. The tool makes the new source branch from the " +
			"target branch, puts all file changes in one commit (the new content of a path, or its removal), and opens " +
			"the merge request. It returns the merge request reference, its URL, the source branch and the creation time. " +
			"It never writes to the target branch and never merges. " +
			"It refuses a source branch that exists, unless that branch has an open merge request into the target branch. " +
			"The tool then returns that merge request and changes nothing. " +
			"Undo: call close_merge_request with the merge_request value of the result.",
		Annotations: kit.Reversible("Open GitLab merge request", true),
	}, v.openMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "close_merge_request",
		Description: "Close one open GitLab merge request with no merge. It does not delete the branch. " +
			"It refuses a merge request that is merged. A merge request that is closed already is not an error. " +
			"This is the undo of open_merge_request. A person can reopen the merge request. No tool of this server reopens it.",
		Annotations: kit.Reversible("Close GitLab merge request", true),
	}, v.closeMergeRequest)
}

func (v *service) closeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.CloseResult, error) {
	out := fixloop.CloseResult{}
	project, iid, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	out.MergeRequest = refKey(project, iid)
	mr, err := v.getMR(ctx, project, iid)
	if err != nil {
		return nil, out, err
	}
	switch mr.State {
	case "merged":
		return nil, out, fmt.Errorf("%w: the merge request is merged, so it cannot be closed", kit.ErrInvalidInput)
	case "closed":
		out.State = fixloop.Closed
		return nil, out, nil
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPut, Path: mrPath(project, iid),
		JSON: map[string]any{"state_event": "close"}, Retry: true, // the same value twice has the same effect
	}, nil)
	if err != nil {
		return nil, out, fmt.Errorf("close the merge request: %w", err)
	}
	out.State, out.Closed = fixloop.Closed, true
	return nil, out, nil
}

func (v *service) openMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.OpenInput) (*mcp.CallToolResult, fixloop.OpenResult, error) {
	out := fixloop.OpenResult{SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch}
	if err := fixloop.CheckOpen(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	if err := kit.CheckString("project", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check project: %w", err)
	}
	if !projectFormat.MatchString(in.Repo) || !fixloop.ValidPath(in.Repo) {
		return nil, out, fmt.Errorf("%w: project must be the path of the project, for example group/project", kit.ErrInvalidInput)
	}
	project := in.Repo

	// The source branch is made from the head of the base branch. When it
	// exists already, the tool accepts it only when its one commit is the
	// commit that this call would make: the same files with the same blobs.
	// A commit of another author, or another change, is refused. The tool never
	// builds on a foreign commit.
	exists, err := v.branchExists(ctx, project, in.SourceBranch)
	if err != nil {
		return nil, out, err
	}
	if exists {
		return v.resume(ctx, project, in, out)
	}
	actions := make([]map[string]any, 0, len(in.Changes))
	for _, c := range in.Changes {
		a, err := v.action(ctx, project, in.TargetBranch, c)
		if err != nil {
			return nil, out, err
		}
		actions = append(actions, a)
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "repository", "commits"),
		JSON: map[string]any{
			"branch": in.SourceBranch, "start_branch": in.TargetBranch,
			"commit_message": in.Title, "actions": actions,
		},
	}, nil)
	var ae *kit.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Body, "already exists") {
		return nil, out, fmt.Errorf("%w: the branch %s exists and has no open merge request into %s. Use another branch name",
			kit.ErrInvalidInput, in.SourceBranch, in.TargetBranch)
	}
	if err != nil {
		return nil, out, fmt.Errorf("create the commit: %w", err)
	}
	var created glMR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "merge_requests"),
		JSON: map[string]any{
			"source_branch": in.SourceBranch, "target_branch": in.TargetBranch,
			"title": in.Title, "description": in.Body, "remove_source_branch": true,
		},
	}, &created)
	if err != nil {
		return nil, out, fmt.Errorf("open the merge request (the branch %s stays, call the tool again): %w", in.SourceBranch, err)
	}
	return nil, v.openResult(project, &created, in, false), nil
}

// openMROf returns the open merge request from the source branch into the
// target branch, or nil when there is none. When the branch exists and has no
// such merge request, the commit call refuses it with "already exists".
func (v *service) openMROf(ctx context.Context, project string, in fixloop.OpenInput) (*glMR, error) {
	var list []glMR
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "merge_requests"),
		Query: url.Values{"source_branch": {in.SourceBranch}, "target_branch": {in.TargetBranch}, "state": {"opened"}, "per_page": {"1"}},
	}, &list)
	if err != nil {
		return nil, fmt.Errorf("look for a merge request of the branch: %w", err)
	}
	if len(list) > 0 {
		return &list[0], nil
	}
	return nil, nil
}

// action makes the commit action of one change. GitLab has no action that
// writes a file whether it exists or not, so the tool asks the target branch.
func (v *service) action(ctx context.Context, project, target string, c fixloop.FileChange) (map[string]any, error) {
	if c.Delete {
		return map[string]any{"action": "delete", "file_path": c.Path}, nil
	}
	op := "update"
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodHead, Path: kit.Path("projects", project, "repository", "files", c.Path),
		Query: url.Values{"ref": {target}},
	}, nil)
	var ae *kit.APIError
	switch {
	case err == nil:
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		op = "create"
	default:
		return nil, fmt.Errorf("look for the file on the target branch: %w", err)
	}
	return map[string]any{"action": op, "file_path": c.Path, "content": *c.Content}, nil
}

func (v *service) openResult(project string, mr *glMR, in fixloop.OpenInput, existing bool) fixloop.OpenResult {
	return fixloop.OpenResult{
		MergeRequest: refKey(project, mr.IID), URL: mr.WebURL,
		SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch, HeadSHA: mr.SHA,
		CreatedAt: fixloop.Time(fixloop.ParseTime(mr.CreatedAt)), Existing: existing,
	}
}

func (v *service) branchExists(ctx context.Context, project, branch string) (bool, error) {
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "branches", branch)}, nil)
	var ae *kit.APIError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("read the source branch: %w", err)
}

// resume handles a source branch that exists. It checks that the branch is
// the work of this tool, then returns its open merge request, or opens one.
func (v *service) resume(ctx context.Context, project string, in fixloop.OpenInput, out fixloop.OpenResult) (*mcp.CallToolResult, fixloop.OpenResult, error) {
	reason, err := v.foreign(ctx, project, in)
	if err != nil {
		return nil, out, err
	}
	if reason != "" {
		return nil, out, fmt.Errorf("%w: the branch %s exists and %s. The tool does not build on it. Use another branch name",
			kit.ErrInvalidInput, in.SourceBranch, reason)
	}
	mr, err := v.openMROf(ctx, project, in)
	if err != nil {
		return nil, out, err
	}
	if mr != nil {
		return nil, v.openResult(project, mr, in, true), nil
	}
	var created glMR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "merge_requests"),
		JSON: map[string]any{
			"source_branch": in.SourceBranch, "target_branch": in.TargetBranch,
			"title": in.Title, "description": in.Body, "remove_source_branch": true,
		},
	}, &created)
	if err != nil {
		return nil, out, fmt.Errorf("open the merge request (the branch %s stays, call the tool again): %w", in.SourceBranch, err)
	}
	return nil, v.openResult(project, &created, in, false), nil
}

// foreign compares the branch with the base branch. It returns an empty reason
// when the branch holds exactly one commit and that commit changes exactly the
// files of the input with the blobs of the input. Otherwise it returns the
// reason.
func (v *service) foreign(ctx context.Context, project string, in fixloop.OpenInput) (string, error) {
	var cmp struct {
		Commits []struct {
			ID string `json:"id"`
		} `json:"commits"`
		Diffs []struct {
			NewPath     string `json:"new_path"`
			RenamedFile bool   `json:"renamed_file"`
			DeletedFile bool   `json:"deleted_file"`
		} `json:"diffs"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "compare"),
		Query: url.Values{"from": {in.TargetBranch}, "to": {in.SourceBranch}},
	}, &cmp)
	if err != nil {
		return "", fmt.Errorf("compare the source branch with %s: %w", in.TargetBranch, err)
	}
	if len(cmp.Commits) != 1 {
		return fmt.Sprintf("holds %d commits that %s does not have, and this tool makes one", len(cmp.Commits), in.TargetBranch), nil
	}
	if len(cmp.Diffs) != len(in.Changes) {
		return "its commit changes other files than the input", nil
	}
	got := make(map[string]bool, len(cmp.Diffs)) // the path, and true for a delete
	for _, d := range cmp.Diffs {
		if d.RenamedFile {
			return "its commit renames " + d.NewPath, nil
		}
		got[d.NewPath] = d.DeletedFile
	}
	for _, c := range in.Changes {
		deleted, ok := got[c.Path]
		switch {
		case !ok:
			return "its commit changes other files than the input", nil
		case c.Delete != deleted:
			return "its commit does not make the change of " + c.Path, nil
		case c.Delete:
			continue
		}
		blob, err := v.blobAt(ctx, project, c.Path, in.SourceBranch)
		if err != nil {
			return "", err
		}
		if blob != fixloop.BlobSHA(*c.Content) {
			return "its commit holds another content for " + c.Path, nil
		}
	}
	return "", nil
}

// blobAt returns the git blob sha of a file at a ref.
func (v *service) blobAt(ctx context.Context, project, path, ref string) (string, error) {
	var f struct {
		BlobID string `json:"blob_id"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "files", path), Query: url.Values{"ref": {ref}},
	}, &f)
	if err != nil {
		return "", fmt.Errorf("read the blob of %s: %w", path, err)
	}
	return f.BlobID, nil
}
