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
//     https://docs.gitlab.com/api/commits/#create-a-commit-with-multiple-files-and-actions
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
			"A person undoes this action by closing the merge request. No tool of this server closes it.",
		Annotations: kit.Reversible("Open GitLab merge request", true),
	}, v.openMergeRequest)
}

func (v *service) openMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.OpenInput) (*mcp.CallToolResult, fixloop.OpenResult, error) {
	out := fixloop.OpenResult{SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch}
	if err := fixloop.CheckOpen(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	if err := kit.CheckString("repo", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check repo: %w", err)
	}
	if !projectFormat.MatchString(in.Repo) || !fixloop.ValidPath(in.Repo) {
		return nil, out, fmt.Errorf("%w: repo must be the path of the project, for example group/project", kit.ErrInvalidInput)
	}
	project := in.Repo

	// A branch from an earlier call that failed after the commit: use its open
	// merge request, if it has one.
	mr, err := v.openMROf(ctx, project, in)
	if err != nil {
		return nil, out, err
	}
	if mr != nil {
		return nil, v.openResult(project, mr, in, true), nil
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
		SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch,
		CreatedAt: fixloop.Time(fixloop.ParseTime(mr.CreatedAt)), Existing: existing,
	}
}
