package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// read_file uses the repository files API:
//   GET /projects/:id/repository/files/:file_path?ref=:ref
// https://docs.gitlab.com/api/repository_files/
// The answer is {"file_path","size","encoding":"base64","content":"<base64>",
// "blob_id":"<blob sha>","ref",...}. A missing file answers 404 with
// {"message":"404 File Not Found"}. A missing project answers 404 with
// {"message":"404 Project Not Found"}, so the tool tells them apart.

func (v *service) registerReadFile(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "read_file",
		Description: "Read one text file of a GitLab project at a ref (a branch, a tag or a commit). Read only. " +
			"The result has found, content and the blob sha of the file. A file that does not exist on the ref gives " +
			"found false with no error. A project that does not exist is an error. " +
			"It refuses a path with .., a leading / or a .git folder, a file over 1 MiB, and a file that is not text.",
		Annotations: kit.ReadOnly("Read GitLab file"),
	}, v.readFile)
}

func (v *service) readFile(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.FileInput) (*mcp.CallToolResult, fixloop.FileResult, error) {
	out := fixloop.FileResult{Path: in.Path, Ref: in.Ref}
	if err := fixloop.CheckFile(in); err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	if err := kit.CheckString("project", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check project: %w", err)
	}
	if !projectFormat.MatchString(in.Repo) || !fixloop.ValidPath(in.Repo) {
		return nil, out, fmt.Errorf("%w: project must be the path of the project, for example group/project", kit.ErrInvalidInput)
	}
	var f struct {
		Size     int    `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		BlobID   string `json:"blob_id"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("projects", in.Repo, "repository", "files", in.Path),
		Query: url.Values{"ref": {in.Ref}},
	}, &f)
	var ae *kit.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && strings.Contains(ae.Body, "File Not Found") {
		return nil, out, nil
	}
	if err != nil {
		return nil, out, fmt.Errorf("read the file: %w", err)
	}
	switch {
	case f.Size > fixloop.MaxReadSize:
		return nil, out, fmt.Errorf("%w: the file is larger than %d bytes", kit.ErrInvalidInput, fixloop.MaxReadSize)
	case f.Encoding != "base64":
		return nil, out, fmt.Errorf("read the file: GitLab gave the encoding %q, not base64", f.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		return nil, out, fmt.Errorf("decode the file: %w", err)
	}
	if !utf8.Valid(data) {
		return nil, out, fmt.Errorf("%w: the file is not text", kit.ErrInvalidInput)
	}
	out.Found, out.Content, out.SHA, out.Size = true, string(data), f.BlobID, len(data)
	return nil, out, nil
}
