package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// changedFiles reads the files of a merge request:
//
//	GET /projects/:id/merge_requests/:iid/diffs?per_page=100
//
// https://docs.gitlab.com/api/merge_requests/#list-merge-request-diffs
// Each item has old_path, new_path, new_file, renamed_file and deleted_file.
// The diffs give no blob id, so the tool reads the blob id of each file that
// exists at the head with
//
//	GET /projects/:id/repository/files/:path?ref=<head sha>
//
// https://docs.gitlab.com/api/repository_files/
// To tell whether more than 100 files exist, the tool reads file number 101
// with per_page=1.
func (v *service) changedFiles(ctx context.Context, project string, iid int, head string) ([]fixloop.ChangedFile, bool, error) {
	var list []struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		NewFile     bool   `json:"new_file"`
		RenamedFile bool   `json:"renamed_file"`
		DeletedFile bool   `json:"deleted_file"`
	}
	path := mrPath(project, iid, "diffs")
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: path, Query: url.Values{"per_page": {strconv.Itoa(fixloop.MaxChangedFiles)}}}, &list)
	if err != nil {
		return nil, false, fmt.Errorf("read the diffs of the merge request: %w", err)
	}
	out := make([]fixloop.ChangedFile, 0, len(list))
	for _, d := range list {
		c := fixloop.ChangedFile{Path: d.NewPath, Change: fixloop.ChangeModified}
		switch {
		case d.DeletedFile:
			c.Path, c.Change = d.OldPath, fixloop.ChangeDeleted
		case d.NewFile:
			c.Change = fixloop.ChangeAdded
		case d.RenamedFile:
			c.Change, c.PreviousPath = fixloop.ChangeRenamed, d.OldPath
		}
		if c.Change != fixloop.ChangeDeleted {
			if c.BlobSHA, err = v.blobAt(ctx, project, c.Path, head); err != nil {
				return nil, false, err
			}
		}
		out = append(out, c)
	}
	truncated := false
	if len(list) >= fixloop.MaxChangedFiles {
		var more []struct {
			NewPath string `json:"new_path"`
		}
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: path,
			Query: url.Values{"per_page": {"1"}, "page": {strconv.Itoa(fixloop.MaxChangedFiles + 1)}},
		}, &more)
		if err != nil {
			return nil, false, fmt.Errorf("read the diffs of the merge request: %w", err)
		}
		truncated = len(more) > 0
	}
	return out, truncated, nil
}
