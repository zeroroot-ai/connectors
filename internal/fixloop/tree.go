package fixloop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The types of a tree entry.
const (
	TreeFile      = "file"
	TreeDir       = "dir"
	TreeSymlink   = "symlink"
	TreeSubmodule = "submodule"
)

// Limits of list_tree.
const (
	// GitHubTreePage is the number of entries that one GitHub call returns.
	GitHubTreePage = 1000
	// GitLabTreePage is the number of entries that one GitLab call returns. It
	// is the per_page of one page of the repository tree.
	GitLabTreePage = 100
	// MaxTreeEntries is the most entries that the tool serves from one tree. It is
	// the limit of the recursive tree of GitHub (100,000 entries). A tree with
	// more entries is cut, and the result has truncated true.
	MaxTreeEntries = 100000
	// MaxCursorLen bounds a page cursor. A page token of GitLab is the base64 of
	// a file name and an object id, so it grows with the path.
	MaxCursorLen = 2000
)

// TreeInput is the argument of list_tree.
type TreeInput struct {
	Repo   string `json:"project" jsonschema:"the repository: owner/repo on GitHub, group/project on GitLab"`
	Ref    string `json:"ref,omitempty" jsonschema:"a branch, a tag or a commit. Leave it out for the default branch."`
	Path   string `json:"path,omitempty" jsonschema:"list only below this folder, for example src/db. Leave it out for the whole repository."`
	Cursor string `json:"cursor,omitempty" jsonschema:"the next_cursor of the last result, to read the next page. Leave it out for the first page."`
}

// TreeEntry is one entry of a tree. Path is the full path from the root of the
// repository. Type is file, dir, symlink or submodule. SHA is the git object
// sha: the blob sha of a file or a symlink, the tree sha of a dir, the commit of
// a submodule. Size is the size in bytes of a file or a symlink. GitLab gives no
// size, so it is absent there. Mode is the git mode: 100644, 100755, 120000,
// 040000 or 160000.
type TreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size,omitempty"`
	Mode string `json:"mode"`
}

// TreeResult is the result of list_tree. NextCursor is empty on the last page.
// Truncated is true when the host or the tool cut the tree, so the list is not
// the whole tree. A caller must not read a truncated list as complete. It lists
// a smaller folder with Path to get a complete answer. Reason says why.
type TreeResult struct {
	Project    string      `json:"project"`
	Ref        string      `json:"ref"`
	Path       string      `json:"path,omitempty"`
	Entries    []TreeEntry `json:"entries"`
	NextCursor string      `json:"next_cursor,omitempty"`
	Truncated  bool        `json:"truncated"`
	Reason     string      `json:"reason,omitempty"`
}

// cursorFormat allows the characters of standard base64 (A-Za-z0-9+/=) and the
// characters of the cursor of the GitHub tool. The cursor is opaque. The server
// checks its meaning.
var cursorFormat = regexp.MustCompile(`^[A-Za-z0-9._:+/=-]+$`)

// CheckTree checks the part of TreeInput that does not depend on the Git host
// and returns the path without a trailing slash.
func CheckTree(in TreeInput) (path string, err error) {
	if in.Ref != "" {
		if err := CheckBranch("ref", in.Ref); err != nil {
			return "", err
		}
	}
	path = strings.TrimSuffix(in.Path, "/")
	if path != "" {
		if err := CheckFilePath(path); err != nil {
			return "", err
		}
	}
	if in.Cursor != "" {
		if err := kit.CheckString("cursor", in.Cursor, MaxCursorLen, cursorFormat); err != nil {
			return "", fmt.Errorf("check cursor: %w", err)
		}
	}
	return path, nil
}

// TreeType maps the git mode and the git object type of an entry (blob, tree
// or commit) to the type of a TreeEntry. It returns "" for a type that it does
// not know.
func TreeType(mode, objectType string) string {
	switch objectType {
	case "tree":
		return TreeDir
	case "commit":
		return TreeSubmodule
	case "blob":
		if mode == "120000" {
			return TreeSymlink
		}
		return TreeFile
	}
	return ""
}

// CursorBinding is a short hash of the project and the other values that name a
// listing (the path, the ref). A page cursor holds it, so a cursor of one listing
// does not work for another project or another path. The project is compared
// without regard to case.
func CursorBinding(project string, rest ...string) string {
	h := sha256.New()
	h.Write([]byte(strings.ToLower(project)))
	for _, r := range rest {
		h.Write([]byte{0})
		h.Write([]byte(r))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
