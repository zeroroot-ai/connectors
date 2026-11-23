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
// https://docs.gitlab.com/api/repositories/#list-repository-tree
// Each entry has id (the git object sha), name, type (tree, blob or commit),
// path (the full path) and mode. It has no size. A missing path answers 404
// (before GitLab 17.7 it answered an empty list). The keyset pagination gives
// the next page in the Link header (rel="next") with a page_token. The page
// does not describe the header, so a full page with no next link is not taken
// for the last page: the tool reports truncated, because it cannot tell. The
// cursor of the tool is the page_token.

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
	if in.Cursor != "" {
		q.Set("page_token", in.Cursor)
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
		typ := fixloop.TreeType(e.Mode, map[string]string{"tree": "tree", "blob": "blob", "commit": "commit"}[e.Type])
		if typ == "" {
			return nil, out, fmt.Errorf("read the tree: GitLab gave the entry type %q for %s", e.Type, e.Path)
		}
		out.Entries = append(out.Entries, fixloop.TreeEntry{Path: e.Path, Type: typ, SHA: e.ID, Mode: e.Mode})
	}
	next, more, err := nextPageToken(hdr.Get("Link"))
	switch {
	case err != nil:
		return nil, out, err
	case more:
		out.NextCursor = next
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
