package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// changedFiles reads the files of a merge request:
//
//	GET /projects/:id/merge_requests/:iid/diffs?per_page=100
//
// https://docs.gitlab.com/api/merge_requests/#list-merge-request-diffs
// Each item has old_path, new_path, a_mode, b_mode, new_file, renamed_file,
// deleted_file, collapsed and too_large. The diffs give no blob id, so the tool
// reads the blob id of each file that exists at the head with
//
//	GET /projects/:id/repository/files/:path?ref=<head sha>
//
// https://docs.gitlab.com/api/repository_files/
//
// Two fields of the merge request itself (https://docs.gitlab.com/api/merge_requests/)
// say how far the diffs can be trusted:
//   - diff_refs.head_sha is the head commit of the latest diff version. The
//     diffs belong to the head only when it equals sha. After a push, GitLab
//     makes the new diff version asynchronously, so the two can differ for a
//     while. The tool then gives no list.
//   - changes_count is a string. It is capped at 1000 and shows "1000+" when
//     the merge request has more changes than GitLab stores. It is empty while
//     GitLab counts. The list is complete only when changes_count is a number
//     that equals the number of listed files. Otherwise it is truncated.
func (v *service) changedFiles(ctx context.Context, project string, iid int, mr *glMR) ([]fixloop.ChangedFile, bool, error) {
	if !strings.EqualFold(mr.DiffRefs.HeadSHA, mr.SHA) {
		return nil, false, fmt.Errorf("the diffs of the merge request belong to the commit %q and not to the head %s. "+
			"GitLab has not made the diff of the head yet", mr.DiffRefs.HeadSHA, mr.SHA)
	}
	if mr.ChangesCount == "" {
		return nil, false, errors.New("GitLab has not counted the changes of the merge request yet")
	}
	var list []struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		AMode       string `json:"a_mode"`
		BMode       string `json:"b_mode"`
		Diff        string `json:"diff"`
		NewFile     bool   `json:"new_file"`
		RenamedFile bool   `json:"renamed_file"`
		DeletedFile bool   `json:"deleted_file"`
	}
	path := mrPath(project, iid, "diffs")
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: path, Query: url.Values{"per_page": {strconv.Itoa(fixloop.MaxChangedFiles)}}}, &list)
	if err != nil {
		return nil, false, fmt.Errorf("read the diffs of the merge request: %w", err)
	}
	truncated := strings.HasSuffix(mr.ChangesCount, "+")
	if !truncated {
		n, perr := strconv.Atoi(mr.ChangesCount)
		if perr != nil {
			return nil, false, fmt.Errorf("GitLab gave the changes count %q, which is not a number", mr.ChangesCount)
		}
		truncated = n != len(list)
	}
	out := make([]fixloop.ChangedFile, 0, len(list))
	for _, d := range list {
		c := fixloop.ChangedFile{Path: d.NewPath, Change: fixloop.ChangeModified, Mode: d.BMode}
		switch {
		case d.DeletedFile:
			c.Path, c.Change, c.Mode = d.OldPath, fixloop.ChangeDeleted, ""
		case d.NewFile:
			c.Change = fixloop.ChangeAdded
		case d.RenamedFile:
			c.Change, c.PreviousPath = fixloop.ChangeRenamed, d.OldPath
		case d.AMode != d.BMode && d.Diff == "":
			c.Change = fixloop.ChangeModeChanged
		}
		if c.Change != fixloop.ChangeDeleted {
			if c.BlobSHA, err = v.blobAt(ctx, project, c.Path, mr.SHA); err != nil {
				return nil, false, err
			}
		}
		out = append(out, c)
	}
	return out, truncated, nil
}
