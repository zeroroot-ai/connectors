package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the Management API of SentinelOne. The reference needs a
// console login, so the shapes come from public sources that call the API:
// the PowerShell module PS-SentinelOne (Invoke-S1AgentAction.ps1) and the
// documentation of the SentinelOne extension of LimaCharlie
// (https://docs.limacharlie.io/5-integrations/extensions/third-party/sentinelone/).
//   - POST /web/api/v2.1/agents/actions/disconnect
//   - POST /web/api/v2.1/agents/actions/connect
//     body {"filter": {"ids": ["<agent id>"]}, "data": {}}
//     answer {"data": {"affected": <n>}}
//   - Header "Authorization: ApiToken <token>".

// agentID is the id of an agent in SentinelOne: a decimal number of 18 or 19
// digits. The pattern is wider so that it holds an id of another length.
var agentID = regexp.MustCompile(`^\d{10,24}$`)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("SENTINELONE_CONSOLE_URL")
	token := env.Secret("SENTINELONE_API_TOKEN")
	timeout := env.Duration("SENTINELONE_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("SENTINELONE_CONSOLE_URL must use https")
	}
	client := kit.NewClient(base, kit.ClientOptions{
		Auth: kit.HeaderAuth("Authorization", "ApiToken ", token), Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "disconnect_from_network",
		Description: "Response action. Disconnect one SentinelOne agent from the network. The endpoint keeps its link " +
			"to the SentinelOne console and loses other network traffic. " +
			"Undo: call reconnect_to_network with the same agent_id.",
		Annotations: kit.Reversible("Disconnect SentinelOne agent from network", true),
	}, v.disconnect)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "reconnect_to_network",
		Description: "Response action. Reconnect one disconnected SentinelOne agent to the network. " +
			"This is the undo of disconnect_from_network.",
		Annotations: kit.Reversible("Reconnect SentinelOne agent to network", true),
	}, v.reconnect)
}

type agentInput struct {
	AgentID string `json:"agent_id" jsonschema:"SentinelOne agent id: a decimal number such as 225494730938493804"`
}

type actionOutput struct {
	AgentID  string `json:"agent_id"`
	Action   string `json:"action"`
	Affected int    `json:"affected"`
	Note     string `json:"note,omitempty"`
}

func (v *service) disconnect(ctx context.Context, _ *mcp.CallToolRequest, in agentInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.act(ctx, in, "disconnect")
}

func (v *service) reconnect(ctx context.Context, _ *mcp.CallToolRequest, in agentInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.act(ctx, in, "connect")
}

func (v *service) act(ctx context.Context, in agentInput, op string) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{AgentID: in.AgentID, Action: op}
	if err := kit.CheckString("agent_id", in.AgentID, 24, agentID); err != nil {
		return nil, out, err
	}
	var resp struct {
		Data struct {
			Affected int `json:"affected"`
		} `json:"data"`
		Errors []struct {
			Code   int    `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost,
		Path:   kit.Path("web", "api", "v2.1", "agents", "actions", op),
		JSON: map[string]any{
			"filter": map[string]any{"ids": []string{in.AgentID}},
			"data":   map[string]any{},
		},
		Retry: true, // the same action twice has the same effect
	}, &resp)
	if err != nil {
		var ae *kit.APIError
		if errors.As(err, &ae) && kit.DecodeErrorBody(ae, &resp) == nil && len(resp.Errors) > 0 {
			e := resp.Errors[0]
			return nil, out, fmt.Errorf("%s: SentinelOne answered HTTP %d: %s: %s", op, ae.Status, e.Title, e.Detail)
		}
		return nil, out, fmt.Errorf("%s: %w", op, err)
	}
	out.Affected = resp.Data.Affected
	if out.Affected == 0 {
		out.Note = "SentinelOne changed no agent. The id is unknown, or the agent is already in this state."
	}
	return nil, out, nil
}
