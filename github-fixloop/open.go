package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// open_merge_request uses the git data API, so one commit holds all file
// changes, and the tool makes no clone:
//   - get a reference, get a commit:      https://docs.github.com/en/rest/git/refs and .../git/commits
//   - create a blob (base64 content):     https://docs.github.com/en/rest/git/blobs#create-a-blob
//   - create a tree (base_tree, entries with a sha, or sha null to remove a
//     path):                              https://docs.github.com/en/rest/git/trees#create-a-tree
//   - create a commit, create a reference: https://docs.github.com/en/rest/git/commits#create-a-commit
//     and https://docs.github.com/en/rest/git/refs#create-a-reference
//   - create a pull request:              https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request
//
// The token needs the permissions contents:write and pull_requests:write.

const fileMode = "100644" // GitHub tree entries of a regular, not executable file

func (v *service) registerOpen(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "open_merge_request",
		Description: "Open one GitHub pull request that holds a change. The tool makes the new source branch from the " +
			"target branch, puts all file changes in one commit (the new content of a path, or its removal), and opens " +
			"the pull request. It returns the pull request reference, its URL, the source branch and the creation time. " +
			"It writes files with the mode of a regular file. It never writes to the target branch and never merges. " +
			"It refuses a source branch that exists, unless that branch has an open pull request into the target branch. " +
			"The tool then returns that pull request and changes nothing. " +
			"A person undoes this action by closing the pull request. No tool of this server closes it.",
		Annotations: kit.Reversible("Open GitHub pull request", true),
	}, v.openMergeRequest)
}

type blobAnswer struct {
	SHA string `json:"sha"`
}

func (v *service) openMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.OpenInput) (*mcp.CallToolResult, fixloop.OpenResult, error) {
	out := fixloop.OpenResult{SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch}
	if err := fixloop.CheckOpen(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	owner, repo, ok := strings.Cut(in.Repo, "/")
	if err := kit.CheckString("repo", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check repo: %w", err)
	}
	if !ok || !ownerFormat.MatchString(owner) || !validRepo(repo) {
		return nil, out, fmt.Errorf("%w: repo must be owner/repo", kit.ErrInvalidInput)
	}
	r := prRef{owner: owner, repo: repo}

	// A branch from an earlier call that failed after it made the branch: use
	// its open pull request, if it has one.
	var existing gitRef
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: refPath(r, in.SourceBranch)}, &existing)
	var ae *kit.APIError
	switch {
	case err == nil:
		pr, ferr := v.findPR(ctx, r, in.SourceBranch)
		if ferr != nil {
			return nil, out, ferr
		}
		if pr == nil || pr.State != "open" || pr.Base.Ref != in.TargetBranch {
			return nil, out, fmt.Errorf("%w: the branch %s exists and has no open pull request into %s. Use another branch name",
				kit.ErrInvalidInput, in.SourceBranch, in.TargetBranch)
		}
		return nil, v.openResult(r, pr, in, true), nil
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
	default:
		return nil, out, fmt.Errorf("read the source branch: %w", err)
	}

	baseSHA, treeSHA, err := v.baseOf(ctx, r, in.TargetBranch)
	if err != nil {
		return nil, out, err
	}
	tree, err := v.treeEntries(ctx, r, in.Changes)
	if err != nil {
		return nil, out, err
	}
	var newTree blobAnswer
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "git", "trees"),
		JSON: map[string]any{"base_tree": treeSHA, "tree": tree},
	}, &newTree)
	if err != nil {
		return nil, out, fmt.Errorf("create the tree: %w", err)
	}
	var commit gitCommit
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "git", "commits"),
		JSON: map[string]any{"message": in.Title, "tree": newTree.SHA, "parents": []string{baseSHA}},
	}, &commit)
	if err != nil {
		return nil, out, fmt.Errorf("create the commit: %w", err)
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "git", "refs"),
		JSON: map[string]any{"ref": "refs/heads/" + in.SourceBranch, "sha": commit.SHA},
	}, nil)
	if err != nil {
		return nil, out, fmt.Errorf("create the source branch: %w", err)
	}
	var created ghPR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "pulls"),
		JSON: map[string]any{"title": in.Title, "head": in.SourceBranch, "base": in.TargetBranch, "body": in.Body},
	}, &created)
	if err != nil {
		return nil, out, fmt.Errorf("open the pull request (the branch %s stays, call the tool again): %w", in.SourceBranch, err)
	}
	return nil, v.openResult(r, &created, in, false), nil
}

func (v *service) openResult(r prRef, pr *ghPR, in fixloop.OpenInput, existing bool) fixloop.OpenResult {
	return fixloop.OpenResult{
		MergeRequest: prRef{r.owner, r.repo, pr.Number}.key(), URL: pr.HTMLURL,
		SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch,
		CreatedAt: fixloop.Time(fixloop.ParseTime(pr.CreatedAt)), Existing: existing,
	}
}

// baseOf returns the commit and the tree that the target branch has now.
func (v *service) baseOf(ctx context.Context, r prRef, target string) (commit, tree string, err error) {
	var tip gitRef
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: refPath(r, target)}, &tip); err != nil {
		return "", "", fmt.Errorf("read the target branch: %w", err)
	}
	var c gitCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", tip.Object.SHA)}, &c); err != nil {
		return "", "", fmt.Errorf("read the commit of the target branch: %w", err)
	}
	if tip.Object.SHA == "" || c.Tree.SHA == "" {
		return "", "", errors.New("GitHub gave no commit or tree for the target branch")
	}
	return tip.Object.SHA, c.Tree.SHA, nil
}

// treeEntries makes one blob for each new content and one entry for each
// change. An entry with a null sha removes the path.
func (v *service) treeEntries(ctx context.Context, r prRef, changes []fixloop.FileChange) ([]map[string]any, error) {
	entries := make([]map[string]any, 0, len(changes))
	for i, c := range changes {
		if c.Delete {
			entries = append(entries, map[string]any{"path": c.Path, "mode": fileMode, "type": "blob", "sha": nil})
			continue
		}
		var b blobAnswer
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodPost, Path: repoPath(r, "git", "blobs"),
			JSON: map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(*c.Content)), "encoding": "base64"},
		}, &b)
		if err != nil {
			return nil, fmt.Errorf("create the blob of change %s: %w", strconv.Itoa(i+1), err)
		}
		entries = append(entries, map[string]any{"path": c.Path, "mode": fileMode, "type": "blob", "sha": b.SHA})
	}
	return entries, nil
}
