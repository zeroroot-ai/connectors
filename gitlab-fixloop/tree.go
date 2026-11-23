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

// list_tree uses the repository tree API:
//   GET /projects/:id/repository/tree?ref=&path=&recursive=true&pagination=keyset&per_page=100&page_token=
// https://docs.gitlab.com/api/repositories/
// Each entry has id (the git object sha), name, type (tree, blob or commit),
// path (the full path) and mode. It has no size. A missing path answers 404
// (before GitLab 17.7 it answered an empty list). The keyset pagination gives
// the next page in the Link header (rel="next") with a page_token. The page
// does not describe the header, so a full page with no next link is not taken
// for the last page: the tool reports truncated, because it cannot tell. The
// page_token is the base64 of a file name and an object id (Gitaly), so it has
// the characters + / and =, and it grows with the path. The cursor of the tool is
// "<hash>:<page_token>". The hash binds the cursor to the project, the path and
// the ref, and the tool refuses a cursor of another listing.

// treePage is the number of entries of one page. A test sets it.
var treePage = fixloop.GitLabTreePage

func (v *service) registerTree(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_tree",
		Description: "List the files and folders of a GitLab project at a ref (a branch or a tag), with the path, the type " +
			"(file, dir, symlink or submodule), the blob sha and the mode of each entry. GitLab gives no size. Read only. The ref is the default branch " +
			"when empty. The path limits the list to one folder. The tool lists every level below it. " +
			"A page has at most 100 entries. When next_cursor is not empty, call the tool again with the same input and that cursor. " +
			"When truncated is true, the tool cannot tell that the list is complete, and the list must not be read as complete. " +
			"Then list a smaller folder with path.",
		Annotations: kit.ReadOnly("List GitLab tree"),
	}, v.listTree)
}

type glTreeEntry struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
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
	if !projectFormat.MatchString(in.Repo) || !fixloop.ValidPath(in.Repo) {
		return nil, out, fmt.Errorf("%w: project must be the path of the project, for example group/project", kit.ErrInvalidInput)
	}
	if out.Ref == "" {
		var info struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", in.Repo)}, &info); err != nil {
			return nil, out, fmt.Errorf("read the project (the project may not exist): %w", err)
		}
		if info.DefaultBranch == "" {
			return nil, out, errors.New("read the project: GitLab gave no default branch")
		}
		out.Ref = info.DefaultBranch
	}
	q := url.Values{
		"ref": {out.Ref}, "recursive": {"true"}, "pagination": {"keyset"}, "per_page": {strconv.Itoa(treePage)},
	}
	if path != "" {
		q.Set("path", path)
	}
	bind := fixloop.CursorBinding(in.Repo, path, out.Ref)
	if in.Cursor != "" {
		token, err := unwrapCursor(in.Cursor, bind)
		if err != nil {
			return nil, out, err
		}
		q.Set("page_token", token)
	}
	var list []glTreeEntry
	var hdr http.Header
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", in.Repo, "repository", "tree"), Query: q, ResponseHeader: &hdr,
	}, &list)
	if err != nil {
		return nil, out, fmt.Errorf("read the tree (the path or the ref may not exist): %w", err)
	}
	for _, e := range list {
		typ := fixloop.TreeType(e.Mode, e.Type)
		if typ == "" {
			return nil, out, fmt.Errorf("read the tree: GitLab gave the entry type %q for %s", e.Type, e.Path)
		}
		out.Entries = append(out.Entries, fixloop.TreeEntry{Path: e.Path, Type: typ, SHA: e.ID, Mode: e.Mode})
	}
	if len(list) == 0 && path != "" && in.Cursor == "" {
		// A missing folder (GitLab before 17.7) and a file both give an empty list. A
		// folder of git is never empty, so check that the path is a folder.
		return nil, out, v.emptyFolderError(ctx, in.Repo, out.Ref, path)
	}
	next, more, err := nextPageToken(hdr.Get("Link"))
	switch {
	case err != nil:
		return nil, out, err
	case more:
		out.NextCursor = bind + ":" + next
	case len(list) >= treePage:
		// A full page and no next link: the tool cannot tell the last page from a cut.
		out.Truncated = true
		out.Reason = "GitLab gave a full page and no link to the next page. List a smaller folder with path."
	}
	return nil, out, nil
}

// nextPageToken returns the page_token of the rel="next" link of a Link header.
// more is false when there is no next link. A next link with no page_token is an
// error, because the tool could not read the rest of the tree.
func nextPageToken(link string) (token string, more bool, err error) {
	for _, part := range strings.Split(link, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 || !strings.Contains(seg[1], `rel="next"`) {
			continue
		}
		u, perr := url.Parse(strings.Trim(strings.TrimSpace(seg[0]), "<>"))
		if perr != nil || u.Query().Get("page_token") == "" {
			return "", true, errors.New("read the tree: GitLab gave a next page link that has no page_token, " +
				"so the tool cannot read the rest of the tree")
		}
		return u.Query().Get("page_token"), true, nil
	}
	return "", false, nil
}

// maxParentPages bounds the pages of the parent folder that checkFolder reads.
const maxParentPages = 10

// emptyFolderError explains an empty list for a path. It always returns an
// error, because the list of a real folder is never empty. It lists the parent
// folder without recursion and looks for the entry of the path, so the error says
// whether the path is a file or is missing.
func (v *service) emptyFolderError(ctx context.Context, project, ref, path string) error {
	parent := ""
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parent = path[:i]
	}
	token := ""
	for range maxParentPages {
		q := url.Values{"ref": {ref}, "pagination": {"keyset"}, "per_page": {strconv.Itoa(fixloop.GitLabTreePage)}}
		if parent != "" {
			q.Set("path", parent)
		}
		if token != "" {
			q.Set("page_token", token)
		}
		var list []glTreeEntry
		var hdr http.Header
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: kit.Path("projects", project, "repository", "tree"), Query: q, ResponseHeader: &hdr,
		}, &list)
		if err != nil {
			return fmt.Errorf("check that %s is a folder (the path or the ref may not exist): %w", path, err)
		}
		for _, e := range list {
			if e.Path != path {
				continue
			}
			if e.Type == "tree" {
				return fmt.Errorf("read the tree: GitLab gave an empty list for the folder %s, so the tool cannot tell that the list is complete", path)
			}
			return fmt.Errorf("%w: %s is a file, not a folder", kit.ErrInvalidInput, path)
		}
		next, more, err := nextPageToken(hdr.Get("Link"))
		if err != nil {
			return err
		}
		if !more {
			return fmt.Errorf("%w: %s is not a folder at the ref %s", kit.ErrInvalidInput, path, ref)
		}
		token = next
	}
	return fmt.Errorf("%w: the tool could not find %s in the parent folder, so it cannot tell that it is a folder", kit.ErrInvalidInput, path)
}

// unwrapCursor reads "<hash>:<page_token>" and returns the page_token. It refuses
// a cursor whose hash is not bind. A page_token is base64, so it has no colon.
func unwrapCursor(cursor, bind string) (string, error) {
	hash, token, ok := strings.Cut(cursor, ":")
	if !ok || token == "" {
		return "", fmt.Errorf("%w: the cursor is not a cursor of list_tree", kit.ErrInvalidInput)
	}
	if hash != bind {
		return "", fmt.Errorf("%w: the cursor is of another project, another ref or another path. Start again without a cursor", kit.ErrInvalidInput)
	}
	return token, nil
}
