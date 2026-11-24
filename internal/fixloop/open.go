package fixloop

import (
	"crypto/sha1" //nolint:gosec // git names a blob with SHA-1, this is no security use
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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

// OpenInput is the argument of open_merge_request. The names follow the
// contract of the incident agent (README of zeroroot-ai/incident-agent): the
// same branch in two calls opens one merge request, and a file with content is
// written whether it exists or not.
type OpenInput struct {
	Repo         string       `json:"project" jsonschema:"the repository: owner/repo on GitHub, group/project on GitLab"`
	SourceBranch string       `json:"branch" jsonschema:"the new branch that holds the changes. It must not exist, unless it has an open merge request from an earlier call."`
	TargetBranch string       `json:"base_branch" jsonschema:"the branch that the merge request merges into. It must exist."`
	Title        string       `json:"title" jsonschema:"title of the merge request and message of the commit"`
	Body         string       `json:"description,omitempty" jsonschema:"description of the merge request"`
	Changes      []FileChange `json:"files" jsonschema:"the file changes of the one commit: 1 to 100 files"`
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
	// HeadSHA is the head commit of the merge request. A caller passes it as
	// expected_head_sha to merge_merge_request, so the host refuses the merge
	// when the head moved.
	HeadSHA string `json:"head_sha"`
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
	if err := CheckBranch("branch", in.SourceBranch); err != nil {
		return err
	}
	if err := CheckBranch("base_branch", in.TargetBranch); err != nil {
		return err
	}
	if in.SourceBranch == in.TargetBranch {
		return fmt.Errorf("%w: branch and base_branch must differ", kit.ErrInvalidInput)
	}
	if err := kit.CheckString("title", in.Title, MaxTitleLen, nil); err != nil {
		return fmt.Errorf("check title: %w", err)
	}
	if len(in.Body) > MaxBodyLen {
		return fmt.Errorf("%w: description is longer than %d bytes", kit.ErrInvalidInput, MaxBodyLen)
	}
	if err := kit.CheckRange("number of files", len(in.Changes), 1, MaxChanges); err != nil {
		return fmt.Errorf("check changes: %w", err)
	}
	seen := map[string]bool{}
	total := 0
	for _, c := range in.Changes {
		if err := CheckFilePath(c.Path); err != nil {
			return err
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

// CheckComment refuses a comment that is too long or is not valid UTF-8. An
// empty comment is allowed.
func CheckComment(c string) error {
	if c == "" {
		return nil
	}
	return kit.CheckString("comment", c, MaxCommentLen, nil)
}

// CloseResult is the result of close_merge_request. Closed is true when the
// tool closed the merge request. It is false when the merge request was
// closed already, and the tool changed nothing.
type CloseResult struct {
	MergeRequest string `json:"merge_request"`
	State        string `json:"state"`
	Closed       bool   `json:"closed"`
}

// CheckFilePath refuses a path that is not a plain relative path in a
// repository: empty, too long, with a leading /, a backslash, an empty folder
// name, a . or .. folder, or a .git folder.
func CheckFilePath(p string) error {
	if err := kit.CheckString("path", p, MaxPathLen, nil); err != nil {
		return fmt.Errorf("check path: %w", err)
	}
	if !ValidPath(p) || strings.Contains(p, "\\") {
		return fmt.Errorf("%w: path is not a plain relative path", kit.ErrInvalidInput)
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".git") {
			return fmt.Errorf("%w: path has a .git folder", kit.ErrInvalidInput)
		}
	}
	return nil
}

// FileInput is the argument of read_file. The names follow the contract of
// the incident agent.
type FileInput struct {
	Repo string `json:"project" jsonschema:"the repository: owner/repo on GitHub, group/project on GitLab"`
	Ref  string `json:"ref" jsonschema:"a branch, a tag or a commit"`
	Path string `json:"path" jsonschema:"path of the file in the repository, with / between folders"`
}

// FileResult is the result of read_file. Found is always present. When the
// file does not exist on the ref, Found is false and the other fields are
// empty. A missing project is an error, not a missing file.
type FileResult struct {
	Found   bool   `json:"found"`
	Content string `json:"content"`
	// SHA is the blob sha of the file.
	SHA  string `json:"sha,omitempty"`
	Size int    `json:"size,omitempty"`
	Path string `json:"path"`
	Ref  string `json:"ref"`
}

// MaxReadSize is the largest file that read_file returns.
const MaxReadSize = 1 << 20

// CheckFile checks the part of FileInput that does not depend on the Git
// host. The server checks the project with the format of its host.
func CheckFile(in FileInput) error {
	if err := CheckBranch("ref", in.Ref); err != nil {
		return err
	}
	return CheckFilePath(in.Path)
}

// BlobSHA returns the git blob sha of a text: the SHA-1 of "blob <length>",
// a zero byte and the text. A host gives the same sha for a file with this
// content, so a caller can compare it with the BlobSHA of a changed file.
func BlobSHA(content string) string {
	h := sha1.New() //nolint:gosec // git names a blob with SHA-1, this is no security use
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// BranchStays returns the text of a failure to open a request after the branch
// was made: "the branch X stays", and ", call the tool again" when a call again
// can finish the work. An HTTP 4xx answer other than 429 is a refusal, unless the
// vendor sent a rate limit as that answer (GitHub sends it as a 403, and the client
// sets RateLimited from the headers). A call
// again fails the same way, so the text does not tell the caller to repeat.
func BranchStays(branch string, err error) string {
	var ae *kit.APIError
	if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests && !ae.RateLimited {
		return "the branch " + branch + " stays"
	}
	return "the branch " + branch + " stays, call the tool again"
}
