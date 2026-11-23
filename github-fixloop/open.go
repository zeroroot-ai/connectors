package main

import (
	"context"
	"encoding/base64"
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

const (
	fileMode = "100644" // GitHub tree entries of a regular, not executable file
	execMode = "100755" // an executable file. The tool keeps it for a path that has it in the base tree.
)

func (v *service) registerOpen(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "open_merge_request",
		Description: "Open one GitHub pull request that holds a change. The tool makes the new source branch from the " +
			"target branch, puts all file changes in one commit (the new content of a path, or its removal), and opens " +
			"the pull request. It returns the pull request reference, its URL, the source branch and the creation time. " +
			"It writes files with the mode of a regular file. It never writes to the target branch and never merges. " +
			"It refuses a source branch that exists, unless that branch has an open pull request into the target branch. " +
			"The tool then returns that pull request and changes nothing. " +
			"Undo: call close_merge_request with the merge_request value of the result.",
		Annotations: kit.Reversible("Open GitHub pull request", true),
	}, v.openMergeRequest)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "close_merge_request",
		Description: "Close one open GitHub pull request with no merge. It does not delete the branch. " +
			"It refuses a pull request that is merged. A pull request that is closed already is not an error. " +
			"This is the undo of open_merge_request. A person can reopen the pull request. No tool of this server reopens it.",
		Annotations: kit.Reversible("Close GitHub pull request", true),
	}, v.closeMergeRequest)
}

func (v *service) closeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.MergeRequestInput) (*mcp.CallToolResult, fixloop.CloseResult, error) {
	out := fixloop.CloseResult{}
	r, err := v.parseRef(in.MergeRequest)
	if err != nil {
		return nil, out, err
	}
	out.MergeRequest = r.key()
	pr, err := v.getPR(ctx, r)
	if err != nil {
		return nil, out, err
	}
	switch {
	case pr.Merged:
		return nil, out, fmt.Errorf("%w: the pull request is merged, so it cannot be closed", kit.ErrInvalidInput)
	case pr.State == "closed":
		out.State = fixloop.Closed
		return nil, out, nil
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPatch, Path: repoPath(r, "pulls", strconv.Itoa(r.number)),
		JSON: map[string]any{"state": "closed"}, Retry: true, // the same value twice has the same effect
	}, nil)
	if err != nil {
		return nil, out, fmt.Errorf("close the pull request: %w", err)
	}
	out.State, out.Closed = fixloop.Closed, true
	return nil, out, nil
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
	if err := kit.CheckString("project", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check project: %w", err)
	}
	if !ok || !ownerFormat.MatchString(owner) || !validRepo(repo) {
		return nil, out, fmt.Errorf("%w: project must be owner/repo", kit.ErrInvalidInput)
	}
	r := prRef{owner: owner, repo: repo}

	// The source branch is made from the head of the base branch. When it
	// exists already, the tool accepts it only when its one commit is the
	// commit that this call would make: the same files with the same blobs.
	// A commit of another author, or another change, is refused. The tool never
	// builds on a foreign commit.
	var existing gitRef
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: refPath(r, in.SourceBranch)}, &existing)
	var ae *kit.APIError
	switch {
	case err == nil:
		return v.resume(ctx, r, in, out, existing.Object.SHA)
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
	default:
		return nil, out, fmt.Errorf("read the source branch: %w", err)
	}

	baseSHA, treeSHA, err := v.baseOf(ctx, r, in.TargetBranch)
	if err != nil {
		return nil, out, err
	}
	modes, err := v.modesOf(ctx, r, treeSHA, in.Changes)
	if err != nil {
		return nil, out, err
	}
	tree, err := v.treeEntries(ctx, r, in.Changes, modes)
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
		return nil, out, kit.Retryable(fmt.Errorf("open the pull request (the branch %s stays, call the tool again): %w", in.SourceBranch, err))
	}
	if err := sameHead(&created, commit.SHA); err != nil {
		return nil, out, err
	}
	return nil, v.openResult(r, &created, in, commit.SHA, false), nil
}

// sameHead refuses a pull request whose head is not the commit that the tool
// made or checked. The head_sha of the result is that commit, so a later merge
// with expected_head_sha merges only that commit.
func sameHead(pr *ghPR, commit string) error {
	if !strings.EqualFold(pr.Head.SHA, commit) {
		return fmt.Errorf("%w: the head of the pull request is %s and not %s, the commit that this tool made or checked. "+
			"A push moved the branch. Nothing was merged", kit.ErrInvalidInput, pr.Head.SHA, commit)
	}
	return nil
}

func (v *service) openResult(r prRef, pr *ghPR, in fixloop.OpenInput, head string, existing bool) fixloop.OpenResult {
	return fixloop.OpenResult{
		MergeRequest: prRef{r.owner, r.repo, pr.Number}.key(), URL: pr.HTMLURL,
		SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch, HeadSHA: head,
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

// modeOr returns the mode that the tool writes for a path: the mode in modes, or
// the mode of a regular file.
func modeOr(modes map[string]string, path string) string {
	if m, ok := modes[path]; ok {
		return m
	}
	return fileMode
}

// modesOf checks each path of the changes against the tree and returns the mode
// that the tool writes for each written path. It reads the tree by walking the
// folders of each path without recursion, so a large tree is read in full:
//
//	GET /repos/{owner}/{repo}/git/trees/{tree_sha}
//
// https://docs.github.com/en/rest/git/trees#get-a-tree
//
// A write to a path that is an executable file (100755) keeps 100755, so an edit
// of a script does not drop its executable bit. A path that does not exist gets
// 100644. The tool refuses, with kit.ErrInvalidInput, a write to a path that
// exists and is not a blob with the mode 100644 or 100755 (a symlink, a
// submodule, a folder), a write or a delete below a folder of the path that is
// not a folder, and a delete of a path that is a folder or a submodule, because
// the delete would remove it whole.
func (v *service) modesOf(ctx context.Context, r prRef, rootTree string, changes []fixloop.FileChange) (map[string]string, error) {
	folders := map[string][]ghTreeItem{}
	list := func(sha string) ([]ghTreeItem, error) {
		if items, ok := folders[sha]; ok {
			return items, nil
		}
		var t struct {
			Truncated bool         `json:"truncated"`
			Tree      []ghTreeItem `json:"tree"`
		}
		if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "trees", sha)}, &t); err != nil {
			return nil, fmt.Errorf("read the tree %s to check the paths: %w", sha, err)
		}
		if t.Truncated {
			return nil, fmt.Errorf("the tree %s is too large to check the paths", sha)
		}
		folders[sha] = t.Tree
		return t.Tree, nil
	}
	modes := map[string]string{}
	for _, c := range changes {
		item, err := itemAt(list, rootTree, c.Path)
		if err != nil {
			return nil, err
		}
		if err := checkChange(c, item); err != nil {
			return nil, err
		}
		if !c.Delete {
			modes[c.Path] = modeOf(item)
		}
	}
	return modes, nil
}

type ghTreeItem struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// itemAt walks the folders of path from the root tree and returns the entry of
// the path, or nil when it does not exist. It refuses a path below an entry that
// is not a folder.
func itemAt(list func(string) ([]ghTreeItem, error), root, path string) (*ghTreeItem, error) {
	segs := strings.Split(path, "/")
	sha := root
	for i, seg := range segs {
		items, err := list(sha)
		if err != nil {
			return nil, err
		}
		var found *ghTreeItem
		for j := range items {
			if items[j].Path == seg {
				found = &items[j]
			}
		}
		switch {
		case found == nil:
			return nil, nil
		case i == len(segs)-1:
			return found, nil
		case found.Type != "tree":
			return nil, fmt.Errorf("%w: %s is below %s, which is a %s and not a folder. The tool does not write through it",
				kit.ErrInvalidInput, path, strings.Join(segs[:i+1], "/"), kindOf(found))
		}
		sha = found.SHA
	}
	return nil, nil
}

// kindOf names the kind of an entry for an error message.
func kindOf(it *ghTreeItem) string {
	switch {
	case it.Type == "tree":
		return "folder"
	case it.Type == "commit":
		return "submodule"
	case it.Mode == "120000":
		return "symlink"
	}
	return "file"
}

// checkChange refuses a change that would replace or remove an entry that is not
// a plain file. A write needs a blob with the mode 100644 or 100755. A delete
// needs a blob (100644, 100755 or 120000), never a folder or a submodule.
func checkChange(c fixloop.FileChange, it *ghTreeItem) error {
	if it == nil {
		if c.Delete {
			return fmt.Errorf("%w: %s does not exist in the base branch, so there is nothing to delete. Remove it from the files, or check the path",
				kit.ErrInvalidInput, c.Path)
		}
		return nil
	}
	plain := it.Type == "blob" && (it.Mode == fileMode || it.Mode == execMode)
	switch {
	case c.Delete && it.Type == "blob":
		return nil
	case c.Delete:
		return fmt.Errorf("%w: %s is a %s. A delete would remove it whole, and the tool deletes files only", kit.ErrInvalidInput, c.Path, kindOf(it))
	case !plain:
		return fmt.Errorf("%w: %s is a %s. The tool writes plain files and does not replace it", kit.ErrInvalidInput, c.Path, kindOf(it))
	}
	return nil
}

// modeOf returns the mode to write for an existing plain file, or fileMode for a
// path that does not exist.
func modeOf(it *ghTreeItem) string {
	if it != nil && it.Mode == execMode {
		return execMode
	}
	return fileMode
}

// treeEntries makes one blob for each new content and one entry for each
// change. An entry with a null sha removes the path.
func (v *service) treeEntries(ctx context.Context, r prRef, changes []fixloop.FileChange, modes map[string]string) ([]map[string]any, error) {
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
		entries = append(entries, map[string]any{"path": c.Path, "mode": modeOr(modes, c.Path), "type": "blob", "sha": b.SHA})
	}
	return entries, nil
}

// resume handles a source branch that exists. It checks that the branch is
// the work of this tool and that its head is that commit, then returns its
// open pull request, or opens one. branchHead is the head of the branch that
// the caller read.
func (v *service) resume(ctx context.Context, r prRef, in fixloop.OpenInput, out fixloop.OpenResult, branchHead string) (*mcp.CallToolResult, fixloop.OpenResult, error) {
	commit, reason, err := v.foreign(ctx, r, in)
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
	pr, err := v.findPR(ctx, r, in.SourceBranch)
	if err != nil {
		return nil, out, err
	}
	if pr != nil && pr.State == "open" && pr.Base.Ref == in.TargetBranch && !pr.fromFork() {
		if err := sameHead(pr, commit); err != nil {
			return nil, out, err
		}
		return nil, v.openResult(r, pr, in, commit, true), nil
	}
	var created ghPR
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: repoPath(r, "pulls"),
		JSON: map[string]any{"title": in.Title, "head": in.SourceBranch, "base": in.TargetBranch, "body": in.Body},
	}, &created)
	if err != nil {
		return nil, out, kit.Retryable(fmt.Errorf("open the pull request (the branch %s stays, call the tool again): %w", in.SourceBranch, err))
	}
	if err := sameHead(&created, commit); err != nil {
		return nil, out, err
	}
	return nil, v.openResult(r, &created, in, commit, false), nil
}

// foreign compares the branch with the base branch. It returns an empty reason
// and the commit when the branch holds exactly one commit and that commit
// changes exactly the files of the input with the blobs of the input.
// Otherwise it returns the reason.
func (v *service) foreign(ctx context.Context, r prRef, in fixloop.OpenInput) (commit, reason string, err error) {
	var cmp struct {
		AheadBy int `json:"ahead_by"`
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
		Files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			SHA      string `json:"sha"`
		} `json:"files"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "compare", in.TargetBranch+"..."+in.SourceBranch),
	}, &cmp)
	if err != nil {
		return "", "", fmt.Errorf("compare the source branch with %s: %w", in.TargetBranch, err)
	}
	if cmp.AheadBy != 1 || len(cmp.Commits) != 1 {
		return "", fmt.Sprintf("holds %d commits that %s does not have, and this tool makes one", cmp.AheadBy, in.TargetBranch), nil
	}
	if len(cmp.Files) != len(in.Changes) {
		return "", "its commit changes other files than the input", nil
	}
	got := make(map[string][2]string, len(cmp.Files))
	for _, f := range cmp.Files {
		got[f.Filename] = [2]string{f.Status, f.SHA}
	}
	for _, c := range in.Changes {
		g, ok := got[c.Path]
		// A status "changed" is a change of the mode. This tool keeps the mode of an
		// existing path, so its commit never changes a mode. A branch with a change
		// of mode is foreign.
		switch {
		case !ok:
			return "", "its commit changes other files than the input", nil
		case c.Delete && g[0] != "removed":
			return "", "its commit does not remove " + c.Path, nil
		case !c.Delete && (g[0] != "added" && g[0] != "modified" || g[1] != fixloop.BlobSHA(*c.Content)):
			return "", "its commit holds another content or another mode for " + c.Path, nil
		}
	}
	// The compare answer has no file mode. The tree of the commit has it, so
	// the tool reads the tree and requires the mode 100644 and the type blob
	// for each written path. A mode 100755, a symlink (120000) or a submodule
	// is a change that this tool did not make.
	commit = cmp.Commits[0].SHA
	reason, err = v.checkModes(ctx, r, commit, in)
	if err != nil || reason != "" {
		return "", reason, err
	}
	return commit, "", nil
}

// checkModes reads the tree of the commit and refuses a written path whose
// entry is not a blob with the mode that the tool writes: 100755 when the path
// is an executable file in the parent commit, 100644 otherwise. A tree that
// GitHub cuts cannot be checked, so it is refused.
func (v *service) checkModes(ctx context.Context, r prRef, commit string, in fixloop.OpenInput) (string, error) {
	var c gitCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", commit)}, &c); err != nil {
		return "", fmt.Errorf("read the commit of the source branch: %w", err)
	}
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: repoPath(r, "git", "trees", c.Tree.SHA), Query: url.Values{"recursive": {"1"}},
	}, &tree)
	if err != nil {
		return "", fmt.Errorf("read the tree of the source branch: %w", err)
	}
	if tree.Truncated {
		return "has a tree that is too large for the tool to check the file modes", nil
	}
	// The expected mode of a path is the mode that the tool writes for it: the mode
	// in the parent commit of the commit, which was the head of the base branch.
	if len(c.Parents) != 1 {
		return "has a commit that does not have one parent", nil
	}
	var parent gitCommit
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "commits", c.Parents[0].SHA)}, &parent); err != nil {
		return "", fmt.Errorf("read the parent commit of the source branch: %w", err)
	}
	modes, err := v.modesOf(ctx, r, parent.Tree.SHA, in.Changes)
	if err != nil {
		return "", err
	}
	entries := make(map[string][3]string, len(tree.Tree))
	for _, e := range tree.Tree {
		entries[e.Path] = [3]string{e.Mode, e.Type, e.SHA}
	}
	for _, f := range in.Changes {
		if f.Delete {
			if _, ok := entries[f.Path]; ok {
				return "its commit does not remove " + f.Path, nil
			}
			continue
		}
		e, ok := entries[f.Path]
		if !ok || e[0] != modeOr(modes, f.Path) || e[1] != "blob" || e[2] != fixloop.BlobSHA(*f.Content) {
			return "its commit holds a file mode, a type or a content that this tool did not write for " + f.Path, nil
		}
	}
	return "", nil
}
