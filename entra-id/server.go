package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the Microsoft Graph v1.0 API:
//   - POST  /users/{id}/revokeSignInSessions
//     https://learn.microsoft.com/en-us/graph/api/user-revokesigninsessions
//     (200, body {"@odata.context": "...", "value": true})
//   - PATCH /users/{id} with {"accountEnabled": false|true}
//     https://learn.microsoft.com/en-us/graph/api/user-update
//     (204 No Content)
// The error body of Graph is {"error": {"code": "...", "message": "..."}}.

const (
	defaultGraphURL     = "https://graph.microsoft.com"
	defaultAuthorityURL = "https://login.microsoftonline.com"
	graphScope          = "https://graph.microsoft.com/.default"
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	tenant := env.Require("ENTRA_TENANT_ID")
	clientID := env.Require("ENTRA_CLIENT_ID")
	secret := env.Secret("ENTRA_CLIENT_SECRET")
	graph := env.OptionalURL("ENTRA_GRAPH_URL", defaultGraphURL)
	authority := env.OptionalURL("ENTRA_AUTHORITY_URL", defaultAuthorityURL)
	timeout := env.Duration("ENTRA_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	cc, err := kit.MicrosoftClientCredentials(authority, tenant, clientID, secret, graphScope)
	if err != nil {
		return nil, fmt.Errorf("configure sign-in: %w", err)
	}
	cc.HTTPClient = hc
	// The Graph URL carries the version of the API.
	base := *graph
	base.Path += "/v1.0"
	svc := &service{client: kit.NewClient(&base, kit.ClientOptions{Auth: cc.Authenticator(), Logger: log, Timeout: timeout, HTTPClient: hc})}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "revoke_sign_in_sessions",
		Description: "Response action. Invalidate all refresh tokens and browser session cookies of one Entra ID user. " +
			"The user must sign in again to each application. A delay of a few minutes can follow before the tokens stop. " +
			"An access token that exists stays valid until it expires. It does not block the user: to block sign-in, " +
			"call disable_account. External users are not covered. No tool undoes this action: the user restores " +
			"access by signing in again.",
		Annotations: kit.Irreversible("Revoke Entra ID sign-in sessions", true),
	}, v.revoke)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "disable_account",
		Description: "Response action. Disable one Entra ID account (accountEnabled false). The user cannot sign in. " +
			"This does not end a session that exists: call revoke_sign_in_sessions for that. " +
			"Undo: call enable_account with the same user_id.",
		Annotations: kit.Reversible("Disable Entra ID account", true),
	}, v.disable)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "enable_account",
		Description: "Response action. Enable one Entra ID account (accountEnabled true). This is the undo of disable_account.",
		Annotations: kit.Reversible("Enable Entra ID account", true),
	}, v.enable)
}

type userInput struct {
	UserID string `json:"user_id" jsonschema:"object id of the user: a GUID such as 87d349ed-44d7-43e1-9a83-5f2406dee5bd. A user principal name is not accepted."`
}

type actionOutput struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
	Done   bool   `json:"done"`
}

func (v *service) revoke(ctx context.Context, _ *mcp.CallToolRequest, in userInput) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{UserID: in.UserID, Action: "revoke_sign_in_sessions"}
	if err := kit.CheckString("user_id", in.UserID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	var resp struct {
		Value bool `json:"value"`
	}
	err := v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: kit.Path("users", in.UserID, "revokeSignInSessions"), Retry: true}, &resp)
	if err != nil {
		return nil, out, graphError("revoke sign-in sessions", err)
	}
	if !resp.Value {
		return nil, out, errors.New("revoke sign-in sessions: Graph did not confirm the revoke")
	}
	out.Done = true
	return nil, out, nil
}

func (v *service) disable(ctx context.Context, _ *mcp.CallToolRequest, in userInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.setEnabled(ctx, in, false)
}

func (v *service) enable(ctx context.Context, _ *mcp.CallToolRequest, in userInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.setEnabled(ctx, in, true)
}

func (v *service) setEnabled(ctx context.Context, in userInput, enabled bool) (*mcp.CallToolResult, actionOutput, error) {
	action := "disable_account"
	if enabled {
		action = "enable_account"
	}
	out := actionOutput{UserID: in.UserID, Action: action}
	if err := kit.CheckString("user_id", in.UserID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPatch, Path: kit.Path("users", in.UserID),
		JSON:  map[string]any{"accountEnabled": enabled},
		Retry: true, // the same value twice has the same effect
	}, nil)
	if err != nil {
		return nil, out, graphError(action, err)
	}
	out.Done = true
	return nil, out, nil
}

// graphError shows the Graph error code and message when the answer has them.
func graphError(op string, err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if kit.DecodeErrorBody(ae, &body) == nil && body.Error.Code != "" {
			return fmt.Errorf("%s: Graph answered HTTP %d, %s: %s", op, ae.Status, body.Error.Code, body.Error.Message)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
