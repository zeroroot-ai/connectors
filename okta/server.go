package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the Okta Management API (OpenAPI spec of Okta,
// https://github.com/okta/okta-management-openapi-spec, and the reference at
// https://developer.okta.com/docs/api/openapi/okta-management/management/tag/UserSessions/ and
// https://developer.okta.com/docs/api/openapi/okta-management/management/tag/UserLifecycle/):
//   - DELETE /api/v1/users/{userId}/sessions                (revokeUserSessions, 204)
//   - POST   /api/v1/users/{id}/lifecycle/suspend           (suspendUser, 200)
//   - POST   /api/v1/users/{id}/lifecycle/unsuspend         (unsuspendUser, 200)
// Okta authenticates a request with the header "Authorization: SSWS <token>".

// userIDFormat is the format of an Okta user id: 20 letters and digits. A
// login or an e-mail address is not accepted, so a model cannot hit another
// user through a login that resolves in a surprising way.
var userIDFormat = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("OKTA_ORG_URL")
	token := env.Secret("OKTA_API_TOKEN")
	timeout := env.Duration("OKTA_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("OKTA_ORG_URL must use https")
	}
	client := kit.NewClient(base, kit.ClientOptions{
		Auth:       kit.HeaderAuth("Authorization", "SSWS ", token),
		Logger:     log,
		Timeout:    timeout,
		HTTPClient: hc,
	})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "revoke_user_sessions",
		Description: "Response action. Revoke all Okta sessions of one user, and optionally the OAuth and OpenID Connect " +
			"refresh and access tokens that Okta issued to the user. The user must sign in again on the next request. " +
			"No tool undoes this action: Okta cannot restore a session, and the user restores access by signing in. " +
			"It does not block the user. To block sign-in, call suspend_user. It does not clear the sessions of web " +
			"or native apps.",
		Annotations: kit.Irreversible("Revoke Okta user sessions", true),
	}, v.revokeSessions)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "suspend_user",
		Description: "Response action. Suspend one Okta user. The user must have the status ACTIVE. A suspended user " +
			"cannot sign in, and keeps group and app assignments. Okta ends the Okta session of the user. " +
			"Undo: call unsuspend_user with the same user_id.",
		Annotations: kit.Reversible("Suspend Okta user", true),
	}, v.suspend)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "unsuspend_user",
		Description: "Response action. Return one suspended Okta user to the status ACTIVE. The user must have the " +
			"status SUSPENDED. This is the undo of suspend_user.",
		Annotations: kit.Reversible("Unsuspend Okta user", true),
	}, v.unsuspend)
}

type userInput struct {
	UserID string `json:"user_id" jsonschema:"Okta user id: 20 letters and digits, for example 00ub0oNGTSWTBKOLGLNR"`
}

type revokeInput struct {
	UserID      string `json:"user_id" jsonschema:"Okta user id: 20 letters and digits, for example 00ub0oNGTSWTBKOLGLNR"`
	OAuthTokens bool   `json:"revoke_oauth_tokens,omitempty" jsonschema:"also revoke the OAuth and OpenID Connect tokens of the user. The default is false."`
}

// actionOutput is the result of a response action.
type actionOutput struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
	Done   bool   `json:"done"`
}

func (v *service) revokeSessions(ctx context.Context, _ *mcp.CallToolRequest, in revokeInput) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{UserID: in.UserID, Action: "revoke_user_sessions"}
	if err := kit.CheckString("user_id", in.UserID, 20, userIDFormat); err != nil {
		return nil, out, err
	}
	q := url.Values{}
	if in.OAuthTokens {
		q.Set("oauthTokens", "true")
	}
	err := v.client.Do(ctx, kit.Request{Method: http.MethodDelete, Path: kit.Path("api", "v1", "users", in.UserID, "sessions"), Query: q}, nil)
	if err != nil {
		return nil, out, fmt.Errorf("revoke user sessions: %w", err)
	}
	out.Done = true
	return nil, out, nil
}

func (v *service) suspend(ctx context.Context, _ *mcp.CallToolRequest, in userInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.lifecycle(ctx, in, "suspend")
}

func (v *service) unsuspend(ctx context.Context, _ *mcp.CallToolRequest, in userInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.lifecycle(ctx, in, "unsuspend")
}

func (v *service) lifecycle(ctx context.Context, in userInput, op string) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{UserID: in.UserID, Action: op + "_user"}
	if err := kit.CheckString("user_id", in.UserID, 20, userIDFormat); err != nil {
		return nil, out, err
	}
	err := v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: kit.Path("api", "v1", "users", in.UserID, "lifecycle", op)}, nil)
	if err != nil {
		return nil, out, fmt.Errorf("%s user: %w", op, err)
	}
	out.Done = true
	return nil, out, nil
}
