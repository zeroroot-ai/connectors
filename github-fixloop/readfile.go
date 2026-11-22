package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// read_file uses the contents API:
//   GET /repos/{owner}/{repo}/contents/{path}?ref={ref}
// https://docs.github.com/en/rest/repos/contents#get-repository-content
// For a file the answer is {"type":"file","encoding":"base64","size":n,
// "sha":"<blob sha>","content":"<base64 with new lines>"}. For a folder it is
// an array. A file of 1 MB to 100 MB comes with encoding "none" and no content.
// A missing path answers 404 with {"message":"Not Found"}. A missing
// repository answers the same, so the tool reads the repository to tell them
// apart.

func (v *service) registerReadFile(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "read_file",
		Description: "Read one text file of a GitHub repository at a ref (a branch, a tag or a commit). Read only. " +
			"The result has found, content and the blob sha of the file. A file that does not exist on the ref gives " +
			"found false with no error. A repository that does not exist is an error. " +
			"It refuses a path with .., a leading / or a .git folder, a folder, a file over 1 MiB, and a file that is not text.",
		Annotations: kit.ReadOnly("Read GitHub file"),
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
	owner, repo, ok := strings.Cut(in.Repo, "/")
	if !ok || !ownerFormat.MatchString(owner) || !validRepo(repo) {
		return nil, out, fmt.Errorf("%w: project must be owner/repo", kit.ErrInvalidInput)
	}
	r := prRef{owner: owner, repo: repo}
	raw, err := v.client.DoBytes(ctx, kit.Request{
		Method: http.MethodGet,
		Path:   repoPath(r, append([]string{"contents"}, strings.Split(in.Path, "/")...)...),
		Query:  url.Values{"ref": {in.Ref}},
	})
	var ae *kit.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		// A 404 means a missing path or a missing repository.
		if rerr := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r)}, nil); rerr != nil {
			return nil, out, fmt.Errorf("read the repository (the project may not exist): %w", rerr)
		}
		return nil, out, nil
	}
	if err != nil {
		return nil, out, fmt.Errorf("read the file: %w", err)
	}
	var f struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
		SHA      string `json:"sha"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		// A folder answers an array.
		return nil, out, fmt.Errorf("%w: the path is a folder, not a file", kit.ErrInvalidInput)
	}
	switch {
	case f.Type != "file":
		return nil, out, fmt.Errorf("%w: the path is a %s, not a file", kit.ErrInvalidInput, f.Type)
	case f.Size > fixloop.MaxReadSize:
		return nil, out, fmt.Errorf("%w: the file is larger than %d bytes", kit.ErrInvalidInput, fixloop.MaxReadSize)
	case f.Encoding != "base64":
		return nil, out, fmt.Errorf("read the file: GitHub gave the encoding %q, not base64", f.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return nil, out, fmt.Errorf("decode the file: %w", err)
	}
	if !utf8.Valid(data) {
		return nil, out, fmt.Errorf("%w: the file is not text", kit.ErrInvalidInput)
	}
	out.Found, out.Content, out.SHA, out.Size = true, string(data), f.SHA, len(data)
	return nil, out, nil
}
