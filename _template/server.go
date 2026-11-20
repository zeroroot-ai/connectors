package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// idFormat is the format of an object id of the vendor. Replace it with the
// real format. An id from a model is untrusted input.
var idFormat = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const maxIDLen = 64

// build reads the configuration from the environment and returns the tools.
func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	base := env.URL("__ENVPREFIX___BASE_URL")
	token := env.Secret("__ENVPREFIX___TOKEN")
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	client := kit.NewClient(base, kit.ClientOptions{
		Auth:   kit.HeaderAuth("Authorization", "Bearer ", token),
		Logger: log,
	})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "get_object",
		Description: "Read one object by id. Read only.",
		Annotations: kit.ReadOnly("Get object"),
	}, v.getObject)
}

type getObjectInput struct {
	ID string `json:"id" jsonschema:"the object id"`
}

type getObjectOutput struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

func (v *service) getObject(ctx context.Context, _ *mcp.CallToolRequest, in getObjectInput) (*mcp.CallToolResult, getObjectOutput, error) {
	var out getObjectOutput
	if err := kit.CheckString("id", in.ID, maxIDLen, idFormat); err != nil {
		return nil, out, err
	}
	// Doc of the vendor API: replace this comment with the URL of the reference.
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "objects", in.ID)}, &out)
	if err != nil {
		return nil, out, fmt.Errorf("get object: %w", err)
	}
	return nil, out, nil
}
