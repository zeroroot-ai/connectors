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

// changedFiles reads the files of a pull request:
//
//	GET /repos/{owner}/{repo}/pulls/{n}/files?per_page=100
//
// https://docs.github.com/en/rest/pulls/pulls#list-pull-requests-files
// Each file has filename, status (added, removed, modified, renamed, copied,
// changed, unchanged), sha (the blob sha) and previous_filename. The list has
// at most 3000 files. The tool reads the first 100. To tell whether more
// exist, it reads file number 101 with per_page=1.
func (v *service) changedFiles(ctx context.Context, r prRef) ([]fixloop.ChangedFile, bool, error) {
	var list []struct {
		Filename string `json:"filename"`
		Status   string `json:"status"`
		SHA      string `json:"sha"`
		Previous string `json:"previous_filename"`
	}
	path := repoPath(r, "pulls", strconv.Itoa(r.number), "files")
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: path, Query: url.Values{"per_page": {strconv.Itoa(fixloop.MaxChangedFiles)}}}, &list)
	if err != nil {
		return nil, false, fmt.Errorf("read the files of the pull request: %w", err)
	}
	out := make([]fixloop.ChangedFile, 0, len(list))
	for _, f := range list {
		c := fixloop.ChangedFile{Path: f.Filename, Change: fixloop.ChangeModified, BlobSHA: f.SHA}
		switch f.Status {
		case "added", "copied":
			c.Change = fixloop.ChangeAdded
		case "removed":
			c.Change, c.BlobSHA = fixloop.ChangeDeleted, ""
		case "renamed":
			c.Change, c.PreviousPath = fixloop.ChangeRenamed, f.Previous
		}
		out = append(out, c)
	}
	truncated := false
	if len(list) >= fixloop.MaxChangedFiles {
		var more []struct {
			Filename string `json:"filename"`
		}
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: path,
			Query: url.Values{"per_page": {"1"}, "page": {strconv.Itoa(fixloop.MaxChangedFiles + 1)}},
		}, &more)
		if err != nil {
			return nil, false, fmt.Errorf("read the files of the pull request: %w", err)
		}
		truncated = len(more) > 0
	}
	return out, truncated, nil
}
