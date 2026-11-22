package fixloop

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Limits of open_merge_request. A model gives the input, so each part has a
// limit.
const (
	MaxChanges    = 100
	MaxPathLen    = 1024
	MaxContentLen = 1 << 20 // bytes of one file
	MaxTotalLen   = 8 << 20 // bytes of all files
	MaxTitleLen   = 256
	MaxBodyLen    = 20000
	MaxBranchLen  = 200
)

var branchFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// FileChange is one change of a file: the new content of a path, or the
// removal of the path. Give content or delete, never both. An empty content
// string makes an empty file.
type FileChange struct {
	Path    string  `json:"path" jsonschema:"path of the file in the repository, with / between folders, for example src/main.go"`
	Content *string `json:"content,omitempty" jsonschema:"the new content of the file as text. Leave it out when delete is true."`
	Delete  bool    `json:"delete,omitempty" jsonschema:"true removes the file. Leave content out."`
}

// OpenInput is the argument of open_merge_request.
type OpenInput struct {
	Repo         string       `json:"repo" jsonschema:"the repository: owner/repo on GitHub, group/project on GitLab"`
	SourceBranch string       `json:"source_branch" jsonschema:"the new branch that holds the changes. It must not exist, unless it has an open merge request from an earlier call."`
	TargetBranch string       `json:"target_branch" jsonschema:"the branch that the merge request merges into. It must exist."`
	Title        string       `json:"title" jsonschema:"title of the merge request and message of the commit"`
	Body         string       `json:"body,omitempty" jsonschema:"description of the merge request"`
	Changes      []FileChange `json:"changes" jsonschema:"the file changes of the one commit: 1 to 100 files"`
}

// OpenResult is the result of open_merge_request. MergeRequest is the
// reference that the other tools take: owner/repo#12 on GitHub and
// group/project!12 on GitLab.
type OpenResult struct {
	MergeRequest string `json:"merge_request"`
	URL          string `json:"url"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	CreatedAt    string `json:"created_at"`
	// Existing is true when the tool found an open merge request for the
	// branch and changed nothing.
	Existing bool `json:"existing,omitempty"`
}

// CheckBranch refuses a branch name that is not plain: letters, digits and
// . _ / - only, with no empty segment, no segment that starts with a dot or
// ends with .lock, and no "..".
func CheckBranch(field, name string) error {
	if err := kit.CheckString(field, name, MaxBranchLen, branchFormat); err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	bad := !ValidPath(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".")
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("%w: %s is not a plain branch name", kit.ErrInvalidInput, field)
	}
	return nil
}

// CheckOpen checks the parts of OpenInput that do not depend on the Git host.
// The server checks repo with the format of its host.
func CheckOpen(in OpenInput) error {
	if err := CheckBranch("source_branch", in.SourceBranch); err != nil {
		return err
	}
	if err := CheckBranch("target_branch", in.TargetBranch); err != nil {
		return err
	}
	if in.SourceBranch == in.TargetBranch {
		return fmt.Errorf("%w: source_branch and target_branch must differ", kit.ErrInvalidInput)
	}
	if err := kit.CheckString("title", in.Title, MaxTitleLen, nil); err != nil {
		return fmt.Errorf("check title: %w", err)
	}
	if len(in.Body) > MaxBodyLen {
		return fmt.Errorf("%w: body is longer than %d bytes", kit.ErrInvalidInput, MaxBodyLen)
	}
	if err := kit.CheckRange("number of changes", len(in.Changes), 1, MaxChanges); err != nil {
		return fmt.Errorf("check changes: %w", err)
	}
	seen := map[string]bool{}
	total := 0
	for _, c := range in.Changes {
		if err := kit.CheckString("path", c.Path, MaxPathLen, nil); err != nil {
			return fmt.Errorf("check path: %w", err)
		}
		if !ValidPath(c.Path) || strings.Contains(c.Path, "\\") {
			return fmt.Errorf("%w: path is not a plain relative path", kit.ErrInvalidInput)
		}
		for _, seg := range strings.Split(c.Path, "/") {
			if strings.EqualFold(seg, ".git") {
				return fmt.Errorf("%w: path has a .git folder", kit.ErrInvalidInput)
			}
		}
		if seen[c.Path] {
			return fmt.Errorf("%w: path appears twice", kit.ErrInvalidInput)
		}
		seen[c.Path] = true
		if c.Delete == (c.Content != nil) {
			return fmt.Errorf("%w: give content or delete true for each change, not both and not none", kit.ErrInvalidInput)
		}
		if c.Content != nil {
			if len(*c.Content) > MaxContentLen {
				return fmt.Errorf("%w: a file is longer than %d bytes", kit.ErrInvalidInput, MaxContentLen)
			}
			total += len(*c.Content)
		}
	}
	if total > MaxTotalLen {
		return fmt.Errorf("%w: the files are longer than %d bytes together", kit.ErrInvalidInput, MaxTotalLen)
	}
	return nil
}
