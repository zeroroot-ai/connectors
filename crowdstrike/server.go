package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the Falcon API of CrowdStrike. The public reference needs a
// Falcon login, so the shapes come from the open source clients of
// CrowdStrike: github.com/CrowdStrike/falconpy (endpoint PerformActionV2) and
// github.com/CrowdStrike/gofalcon (models MsaEntityActionRequestV2 and
// MsaReplyAffectedEntities):
//   - POST /devices/entities/devices-actions/v2?action_name=contain|lift_containment
//     body {"ids": ["<agent id>"]}
//     answer 202 {"meta": {...}, "errors": [{"code": n, "message": "..."}],
//     "resources": [{"id": "<agent id>", "path": "..."}]}
//   - POST /oauth2/token with the form fields client_id and client_secret
//     answer 201 {"access_token": "...", "expires_in": n, "token_type": "bearer"}

const defaultBaseURL = "https://api.crowdstrike.com"

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.OptionalURL("FALCON_BASE_URL", defaultBaseURL)
	id := env.Require("FALCON_CLIENT_ID")
	secret := env.Secret("FALCON_CLIENT_SECRET")
	timeout := env.Duration("FALCON_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	cc := &kit.ClientCredentials{
		TokenURL: base.String() + "/oauth2/token", ID: id, Secret: secret,
		ClientSecretInBody: true, OmitGrantType: true, HTTPClient: hc,
	}
	svc := &service{client: kit.NewClient(base, kit.ClientOptions{Auth: cc.Authenticator(), Logger: log, Timeout: timeout, HTTPClient: hc})}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "contain_host",
		Description: "Response action. Network-contain one Falcon host. The host stops all network traffic except " +
			"traffic to the CrowdStrike cloud and the IP addresses of the containment policy. " +
			"The request is queued. A host that is offline contains when it connects. " +
			"Undo: call lift_containment with the same host_id.",
		Annotations: kit.Reversible("Contain Falcon host", true),
	}, v.contain)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "lift_containment",
		Description: "Response action. Lift the network containment of one Falcon host. The host returns to normal " +
			"network traffic. This is the undo of contain_host.",
		Annotations: kit.Reversible("Lift Falcon containment", true),
	}, v.lift)
}

type hostInput struct {
	HostID string `json:"host_id" jsonschema:"Falcon agent id (AID) of the host: 32 hexadecimal characters"`
}

type actionOutput struct {
	HostID string `json:"host_id"`
	Action string `json:"action"`
	// Accepted is true when Falcon accepted the request (HTTP 202) for this
	// host. The change takes effect when the sensor of the host gets it.
	Accepted bool `json:"accepted"`
}

func (v *service) contain(ctx context.Context, _ *mcp.CallToolRequest, in hostInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.act(ctx, in, "contain")
}

func (v *service) lift(ctx context.Context, _ *mcp.CallToolRequest, in hostInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.act(ctx, in, "lift_containment")
}

type affected struct {
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Resources []struct {
		ID string `json:"id"`
	} `json:"resources"`
}

func (v *service) act(ctx context.Context, in hostInput, falconAction string) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{HostID: in.HostID, Action: falconAction}
	if err := kit.CheckString("host_id", in.HostID, 32, kit.Hex32); err != nil {
		return nil, out, err
	}
	var resp affected
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost,
		Path:   "/devices/entities/devices-actions/v2",
		Query:  url.Values{"action_name": {falconAction}},
		JSON:   map[string][]string{"ids": {in.HostID}},
		Retry:  true, // the same action twice has the same effect
	}, &resp)
	if err != nil {
		return nil, out, falconError(falconAction, err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return nil, out, fmt.Errorf("%s: Falcon reported: %s", falconAction, strings.Join(msgs, "; "))
	}
	for _, r := range resp.Resources {
		if strings.EqualFold(r.ID, in.HostID) {
			out.Accepted = true
		}
	}
	if !out.Accepted {
		return nil, out, fmt.Errorf("%s: Falcon did not list the host in its answer", falconAction)
	}
	return nil, out, nil
}

func falconError(op string, err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var body affected
		if kit.DecodeErrorBody(ae, &body) == nil && len(body.Errors) > 0 {
			return kit.Described(err, "%s: Falcon answered HTTP %d: %d %s", op, ae.Status, body.Errors[0].Code, body.Errors[0].Message)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
