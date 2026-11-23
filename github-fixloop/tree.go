package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// list_tree uses the git trees API:
//   GET /repos/{owner}/{repo}/git/trees/{tree_sha}?recursive=1
// https://docs.github.com/en/rest/git/trees#get-a-tree
// tree_sha is a sha or a ref name (a branch or a tag). With recursive, the
// answer holds every object below the tree, and the tree array is capped at
// 100,000 entries or 7 MB. The answer then has truncated true. Each entry has
// path, mode (100644, 100755, 040000, 160000, 120000), type (blob, tree,
// commit), sha and size (blobs only). Without recursive the answer has the top
// level only. The tool walks the folders of path without recursive, then reads
// the subtree with recursive. A page cursor is "<tree sha>:<offset>:<hash>", where hash binds the cursor to the project and the path. It names
// the tree by its sha, so the pages of one listing come from one tree even when
// the branch moves.

// treePage is the number of entries of one page. A test sets it.
var treePage = fixloop.GitHubTreePage

func (v *service) registerTree(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_tree",
		Description: "List the files and folders of a GitHub repository at a ref (a branch, a tag or a commit), with the path, the type " +
			"(file, dir, symlink or submodule), the blob sha, the size and the mode of each entry. Read only. The ref is the default branch " +
			"when empty. The path limits the list to one folder. The tool lists every level below it. " +
			"A page has at most 1000 entries. When next_cursor is not empty, call the tool again with the same input and that cursor. " +
			"When truncated is true, GitHub or the tool cut the tree, and the list is not complete. " +
			"Then list a smaller folder with path. The tool never gives a cut list as a complete list.",
		Annotations: kit.ReadOnly("List GitHub tree"),
	}, v.listTree)
}

type ghTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

type ghTree struct {
	SHA       string        `json:"sha"`
	Truncated bool          `json:"truncated"`
	Tree      []ghTreeEntry `json:"tree"`
}

func (v *service) getTree(ctx context.Context, r prRef, ref string, recursive bool) (*ghTree, error) {
	q := url.Values{}
	if recursive {
		q.Set("recursive", "1")
	}
	var t ghTree
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "git", "trees", ref), Query: q}, &t); err != nil {
		if errors.Is(err, kit.ErrAnswerTooLarge) {
			return nil, fmt.Errorf("%w: the tree is too large for one answer. List a smaller folder with path", kit.ErrInvalidInput)
		}
		return nil, fmt.Errorf("read the tree %s: %w", ref, err)
	}
	if t.SHA == "" {
		return nil, errors.New("read the tree: GitHub gave no sha")
	}
	return &t, nil
}

func (v *service) listTree(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.TreeInput) (*mcp.CallToolResult, fixloop.TreeResult, error) {
	out := fixloop.TreeResult{Project: in.Repo, Ref: in.Ref, Entries: []fixloop.TreeEntry{}}
	path, err := fixloop.CheckTree(in)
	if err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	out.Path = path
	if err := kit.CheckString("project", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check project: %w", err)
	}
	owner, repo, ok := strings.Cut(in.Repo, "/")
	if !ok || !ownerFormat.MatchString(owner) || !validRepo(repo) {
		return nil, out, fmt.Errorf("%w: project must be owner/repo", kit.ErrInvalidInput)
	}
	r := prRef{owner: owner, repo: repo}

	bind := cursorBinding(in.Repo, path)
	treeSHA, offset, err := parseTreeCursor(in.Cursor, bind)
	if err != nil {
		return nil, out, err
	}
	if treeSHA == "" {
		if out.Ref, err = v.treeRef(ctx, r, out.Ref); err != nil {
			return nil, out, err
		}
		if treeSHA, err = v.subtreeSHA(ctx, r, out.Ref, path); err != nil {
			return nil, out, err
		}
	}
	t, err := v.getTree(ctx, r, treeSHA, true)
	if err != nil {
		return nil, out, err
	}
	if t.SHA != treeSHA && in.Cursor != "" {
		return nil, out, fmt.Errorf("%w: the cursor does not match the tree", kit.ErrInvalidInput)
	}
	out.Truncated = t.Truncated
	if t.Truncated {
		out.Reason = "GitHub cut the recursive tree at 100000 entries or 7 MB. List a smaller folder with path."
	}
	list := t.Tree
	if len(list) > fixloop.MaxTreeEntries {
		list = list[:fixloop.MaxTreeEntries]
		out.Truncated = true
		out.Reason = "the tree has more than " + strconv.Itoa(fixloop.MaxTreeEntries) + " entries. List a smaller folder with path."
	}
	if offset > len(list) {
		return nil, out, fmt.Errorf("%w: the cursor is past the end of the tree", kit.ErrInvalidInput)
	}
	end := min(offset+treePage, len(list))
	if out.Entries, err = toTreeEntries(list[offset:end], path); err != nil {
		return nil, out, err
	}
	if end < len(list) {
		out.NextCursor = treeSHA + ":" + strconv.Itoa(end) + ":" + bind
	}
	return nil, out, nil
}

// cursorBinding is a short hash of the project and the path. A cursor holds it,
// so a cursor of one listing does not work for another project or another path.
func cursorBinding(project, path string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(project) + "\x00" + path))
	return hex.EncodeToString(sum[:8])
}

// parseTreeCursor reads "<tree sha>:<offset>:<hash>". It refuses a cursor whose
// hash is not bind.
func parseTreeCursor(c, bind string) (sha string, offset int, err error) {
	if c == "" {
		return "", 0, nil
	}
	parts := strings.Split(c, ":")
	if len(parts) != 3 || len(parts[0]) < 40 {
		return "", 0, fmt.Errorf("%w: the cursor is not a cursor of list_tree", kit.ErrInvalidInput)
	}
	offset, perr := strconv.Atoi(parts[1])
	if perr != nil || offset < 0 {
		return "", 0, fmt.Errorf("%w: the cursor is not a cursor of list_tree", kit.ErrInvalidInput)
	}
	if parts[2] != bind {
		return "", 0, fmt.Errorf("%w: the cursor is of another project or another path. Start again without a cursor", kit.ErrInvalidInput)
	}
	return parts[0], offset, nil
}

// subtreeSHA returns the sha of the tree at a path of a ref. For an empty path
// it returns the ref, and GitHub resolves it.
func (v *service) subtreeSHA(ctx context.Context, r prRef, ref, path string) (string, error) {
	if path == "" {
		t, err := v.getTree(ctx, r, ref, false)
		if err != nil {
			return "", err
		}
		return t.SHA, nil
	}
	sha := ref
	for _, seg := range strings.Split(path, "/") {
		t, err := v.getTree(ctx, r, sha, false)
		if err != nil {
			return "", err
		}
		next := ""
		for _, e := range t.Tree {
			if e.Path == seg && e.Type == "tree" {
				next = e.SHA
			}
		}
		if next == "" {
			return "", fmt.Errorf("%w: %s is not a folder at the ref %s", kit.ErrInvalidInput, path, ref)
		}
		sha = next
	}
	return sha, nil
}

// treeRef returns the ref, or the default branch when the ref is empty.
func (v *service) treeRef(ctx context.Context, r prRef, ref string) (string, error) {
	if ref != "" {
		return ref, nil
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r)}, &info); err != nil {
		return "", fmt.Errorf("read the repository (the project may not exist): %w", err)
	}
	if info.DefaultBranch == "" {
		return "", errors.New("read the repository: GitHub gave no default branch")
	}
	return info.DefaultBranch, nil
}

// treeEntries makes the result entries. The prefix is the folder of the listing.
func toTreeEntries(list []ghTreeEntry, prefix string) ([]fixloop.TreeEntry, error) {
	out := make([]fixloop.TreeEntry, 0, len(list))
	for _, e := range list {
		typ := fixloop.TreeType(e.Mode, e.Type)
		if typ == "" {
			return nil, fmt.Errorf("read the tree: GitHub gave the entry type %q for %s", e.Type, e.Path)
		}
		p := e.Path
		if prefix != "" {
			p = prefix + "/" + e.Path
		}
		out = append(out, fixloop.TreeEntry{Path: p, Type: typ, SHA: e.SHA, Size: e.Size, Mode: e.Mode})
	}
	return out, nil
}
