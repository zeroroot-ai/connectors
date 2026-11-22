package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// changedFiles reads the files of a pull request at two commits that the
// caller names, with
//
//	GET /repos/{owner}/{repo}/compare/{base sha}...{head sha}
//
// https://docs.github.com/en/rest/commits/commits#compare-two-commits
// The pull request files endpoint reads the pull request as it is when the call
// runs. It can lag behind a push, and a head that goes from A to B and back to A
// between two reads passes a check of the head. A compare of two commit shas
// names the head, so the files are the files of the head that the state returned.
// Each file has filename, status (added, removed, modified, renamed, copied,
// changed, unchanged), sha (the blob sha at the head) and previous_filename. The
// status changed is a change of the file mode or the type, and the tool maps it
// to mode_changed. GitHub gives at most 300 files in one answer. The tool returns
// the first 100, and truncated is true when the answer has more than 100 files.
// An answer with 300 files can be cut, so it is truncated too.
func (v *service) changedFiles(ctx context.Context, r prRef, base, head string) ([]fixloop.ChangedFile, bool, error) {
	var cmp struct {
		Files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			SHA      string `json:"sha"`
			Previous string `json:"previous_filename"`
		} `json:"files"`
	}
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r, "compare", base+"..."+head)}, &cmp)
	if err != nil {
		return nil, false, fmt.Errorf("compare the base and the head of the pull request: %w", err)
	}
	truncated := len(cmp.Files) > fixloop.MaxChangedFiles || len(cmp.Files) >= githubCompareFileLimit
	list := cmp.Files
	if len(list) > fixloop.MaxChangedFiles {
		list = list[:fixloop.MaxChangedFiles]
	}
	out := make([]fixloop.ChangedFile, 0, len(list))
	for _, f := range list {
		c := fixloop.ChangedFile{Path: f.Filename, Change: fixloop.ChangeModified, BlobSHA: f.SHA}
		switch f.Status {
		case "added", "copied":
			c.Change = fixloop.ChangeAdded
		case "changed":
			c.Change = fixloop.ChangeModeChanged
		case "removed":
			c.Change, c.BlobSHA = fixloop.ChangeDeleted, ""
		case "renamed":
			c.Change, c.PreviousPath = fixloop.ChangeRenamed, f.Previous
		}
		out = append(out, c)
	}
	return out, truncated, nil
}

// githubCompareFileLimit is the largest number of files that GitHub gives in a
// compare answer.
const githubCompareFileLimit = 300
