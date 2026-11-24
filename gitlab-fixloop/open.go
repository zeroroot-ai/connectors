package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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
			"With a comment, it posts the comment on the merge request first, unless a note with the same text is there, and posts nothing on a closed one. " +
			"It refuses a merge request that is merged. A merge request that is closed already is not an error. " +
			"This is the undo of open_merge_request. A person can reopen the merge request. No tool of this server reopens it.",
		Annotations: kit.Reversible("Close GitLab merge request", true),
	}, v.closeMergeRequest)
}

func (v *service) closeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.CloseInput) (*mcp.CallToolResult, fixloop.CloseResult, error) {
	out := fixloop.CloseResult{}
	project, iid, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	if err := fixloop.CheckComment(in.Comment); err != nil {
		return nil, out, fmt.Errorf("check comment: %w", err)
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
	posted := false
	if in.Comment != "" {
		posted, err = v.hasComment(ctx, project, iid, in.Comment)
		if err != nil {
			return nil, out, err
		}
	}
	if in.Comment != "" && !posted {
		// A note is not idempotent, so the client never repeats it. The tool
		// reads the notes first, so a repeat of the whole call after a lost
		// answer posts the comment once.
		err = v.client.Do(ctx, kit.Request{
			Method: http.MethodPost, Path: mrPath(project, iid, "notes"),
			JSON: map[string]any{"body": in.Comment},
		}, nil)
		if err != nil {
			return nil, out, fmt.Errorf("post the comment: %w", err)
		}
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
	head, exists, err := v.branchHead(ctx, project, in.SourceBranch)
	if err != nil {
		return nil, out, err
	}
	if exists {
		return v.resume(ctx, project, in, out, head)
	}
	actions := make([]map[string]any, 0, len(in.Changes))
	for _, c := range in.Changes {
		a, err := v.action(ctx, project, in.TargetBranch, c)
		if err != nil {
			return nil, out, err
		}
		actions = append(actions, a)
	}
	var commit struct {
		ID string `json:"id"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "repository", "commits"),
		JSON: map[string]any{
			"branch": in.SourceBranch, "start_branch": in.TargetBranch,
			"commit_message": in.Title, "actions": actions,
		},
	}, &commit)
	var ae *kit.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Body, "already exists") {
		return nil, out, fmt.Errorf("%w: the branch %s exists and has no open merge request into %s. Use another branch name",
			kit.ErrInvalidInput, in.SourceBranch, in.TargetBranch)
	}
	if err != nil {
		return nil, out, fmt.Errorf("create the commit: %w", err)
	}
	if !kit.Hex40.MatchString(commit.ID) {
		return nil, out, errors.New("create the commit: GitLab gave no commit id")
	}
	created, existing, err := v.createMR(ctx, project, in)
	if err != nil {
		return nil, out, err
	}
	if err := sameHead(created, commit.ID, in.SourceBranch); err != nil {
		return nil, out, err
	}
	return nil, v.openResult(project, created, in, commit.ID, existing), nil
}

// createMR opens the merge request. When GitLab answers 409 or 422, a merge request
// of the branch can exist already, for example from a create whose answer was lost.
// The tool then lists the merge requests of the branch again and returns the open
// one, with existing true.
func (v *service) createMR(ctx context.Context, project string, in fixloop.OpenInput) (mr *glMR, existing bool, err error) {
	var created glMR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("projects", project, "merge_requests"),
		JSON: map[string]any{
			"source_branch": in.SourceBranch, "target_branch": in.TargetBranch,
			"title": in.Title, "description": in.Body, "remove_source_branch": true,
		},
	}, &created)
	if err != nil {
		var ae *kit.APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusUnprocessableEntity) {
			if found, ferr := v.openMROf(ctx, project, in); ferr == nil && found != nil {
				return found, true, nil
			}
		}
		return nil, false, kit.Retryable(fmt.Errorf("open the merge request (%s): %w", fixloop.BranchStays(in.SourceBranch, err), err))
	}
	return &created, false, nil
}

// sameHead refuses a merge request whose head is not the commit that the tool
// made or checked. The head_sha of the result is that commit, so a later merge
// with expected_head_sha merges only that commit.
func sameHead(mr *glMR, commit, branch string) error {
	if !strings.EqualFold(mr.SHA, commit) {
		return fmt.Errorf("%w: the head of the merge request is %s and not %s, the commit that this tool made or checked. "+
			"A push moved the branch. The merge request !%d (%s) is open, and the branch %s stays",
			kit.ErrInvalidInput, mr.SHA, commit, mr.IID, mr.WebURL, branch)
	}
	return nil
}

// openMROf returns the open merge request from the source branch into the
// target branch, or nil when there is none. When the branch exists and has no
// such merge request, the commit call refuses it with "already exists".
func (v *service) openMROf(ctx context.Context, project string, in fixloop.OpenInput) (*glMR, error) {
	var list []glMR
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "merge_requests"),
		Query: url.Values{"source_branch": {in.SourceBranch}, "target_branch": {in.TargetBranch}, "state": {"opened"}, "per_page": {"20"}},
	}, &list)
	if err != nil {
		return nil, fmt.Errorf("look for a merge request of the branch: %w", err)
	}
	// A merge request from a fork can have the same branch name. It is not ours.
	for i := range list {
		if !list[i].fromFork() {
			return &list[i], nil
		}
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

func (v *service) openResult(project string, mr *glMR, in fixloop.OpenInput, head string, existing bool) fixloop.OpenResult {
	return fixloop.OpenResult{
		MergeRequest: refKey(project, mr.IID), URL: mr.WebURL,
		SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch, HeadSHA: head,
		CreatedAt: fixloop.Time(fixloop.ParseTime(mr.CreatedAt)), Existing: existing,
	}
}

// branchHead returns the head commit of a branch, or exists false when the
// branch does not exist.
func (v *service) branchHead(ctx context.Context, project, branch string) (head string, exists bool, err error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err = v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "branches", branch)}, &b)
	var ae *kit.APIError
	switch {
	case err == nil:
		return b.Commit.ID, true, nil
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return "", false, nil
	}
	return "", false, fmt.Errorf("read the source branch: %w", err)
}

// resume handles a source branch that exists. It checks that the branch is
// the work of this tool and that its head is that commit, then returns its
// open merge request, or opens one. branchHead is the head of the branch that
// the caller read.
func (v *service) resume(ctx context.Context, project string, in fixloop.OpenInput, out fixloop.OpenResult, branchHead string) (*mcp.CallToolResult, fixloop.OpenResult, error) {
	commit, reason, err := v.foreign(ctx, project, in)
	if err != nil {
		return nil, out, err
	}
	if reason == "" && !strings.EqualFold(commit, branchHead) {
		reason = "has a head that is not the commit that this tool checked"
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
		if err := sameHead(mr, commit, in.SourceBranch); err != nil {
			return nil, out, err
		}
		return nil, v.openResult(project, mr, in, commit, true), nil
	}
	created, existing, err := v.createMR(ctx, project, in)
	if err != nil {
		return nil, out, err
	}
	if err := sameHead(created, commit, in.SourceBranch); err != nil {
		return nil, out, err
	}
	return nil, v.openResult(project, created, in, commit, existing), nil
}

// foreign compares the branch with the base branch. It returns an empty reason
// and the commit when the branch holds exactly one commit and that commit
// changes exactly the files of the input with the blobs of the input, read at
// that commit. Otherwise it returns the reason.
func (v *service) foreign(ctx context.Context, project string, in fixloop.OpenInput) (commit, reason string, err error) {
	var cmp struct {
		Commits []struct {
			ID string `json:"id"`
		} `json:"commits"`
		Diffs []struct {
			NewPath     string `json:"new_path"`
			AMode       string `json:"a_mode"`
			BMode       string `json:"b_mode"`
			RenamedFile bool   `json:"renamed_file"`
			DeletedFile bool   `json:"deleted_file"`
			NewFile     bool   `json:"new_file"`
		} `json:"diffs"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "compare"),
		Query: url.Values{"from": {in.TargetBranch}, "to": {in.SourceBranch}},
	}, &cmp)
	if err != nil {
		return "", "", fmt.Errorf("compare the source branch with %s: %w", in.TargetBranch, err)
	}
	if len(cmp.Commits) != 1 {
		return "", fmt.Sprintf("holds %d commits that %s does not have, and this tool makes one", len(cmp.Commits), in.TargetBranch), nil
	}
	if len(cmp.Diffs) != len(in.Changes) {
		return "", "its commit changes other files than the input", nil
	}
	type diff struct {
		deleted, isNew bool
		aMode, bMode   string
	}
	got := make(map[string]diff, len(cmp.Diffs))
	for _, d := range cmp.Diffs {
		if d.RenamedFile {
			return "", "its commit renames " + d.NewPath, nil
		}
		got[d.NewPath] = diff{deleted: d.DeletedFile, isNew: d.NewFile, aMode: d.AMode, bMode: d.BMode}
	}
	commit = cmp.Commits[0].ID
	for _, c := range in.Changes {
		d, ok := got[c.Path]
		switch {
		case !ok:
			return "", "its commit changes other files than the input", nil
		case c.Delete != d.deleted:
			return "", "its commit does not make the change of " + c.Path, nil
		case c.Delete:
			continue
		// This tool writes a new file with the mode 100644 and keeps the mode of
		// a file that exists. Another mode is a change that the tool did not make.
		case d.isNew && d.bMode != "100644", !d.isNew && d.aMode != d.bMode:
			return "", "its commit changes the file mode of " + c.Path, nil
		}
		blob, err := v.blobAt(ctx, project, c.Path, commit)
		if err != nil {
			return "", "", err
		}
		if blob != fixloop.BlobSHA(*c.Content) {
			return "", "its commit holds another content for " + c.Path, nil
		}
	}
	return commit, "", nil
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

// hasComment reports whether the merge request holds a note with exactly this
// body. The caller makes the body unique, so a match is the comment of an
// earlier call.
func (v *service) hasComment(ctx context.Context, project string, iid int, body string) (bool, error) {
	for page := 1; page <= fixloop.MaxCommentPages; page++ {
		var list []struct {
			Body string `json:"body"`
		}
		var hdr http.Header
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: mrPath(project, iid, "notes"), ResponseHeader: &hdr,
			// GitLab lists the notes newest first when the order is explicit, so the
			// notes of an earlier call are on the first pages.
			Query: url.Values{
				"per_page": {strconv.Itoa(fixloop.CommentPageSize)}, "page": {strconv.Itoa(page)},
				"order_by": {"created_at"}, "sort": {"desc"},
			},
		}, &list)
		if err != nil {
			return false, fmt.Errorf("read the notes: %w", err)
		}
		for _, c := range list {
			if c.Body == body {
				return true, nil
			}
		}
		// GitLab names the next page in X-Next-Page. A page of exactly 100 notes
		// with no next page is the whole list.
		if hdr.Get("X-Next-Page") == "" {
			return false, nil
		}
	}
	return false, nil
}
