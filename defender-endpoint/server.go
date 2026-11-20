package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the Microsoft Defender for Endpoint API:
//   - POST /api/machines/{id}/isolate
//     https://learn.microsoft.com/en-us/defender-endpoint/api/isolate-machine
//     body {"Comment": "...", "IsolationType": "Full"|"Selective"|"UnManagedDevice"}
//     answer 201 with a machineAction
//   - POST /api/machines/{id}/unisolate
//     https://learn.microsoft.com/en-us/defender-endpoint/api/unisolate-machine
//     body {"Comment": "..."}, answer 201 with a machineAction
//   - machineAction: https://learn.microsoft.com/en-us/defender-endpoint/api/machineaction
// The token comes from the client credentials grant. Microsoft advises the
// scope https://api.securitycenter.microsoft.com/.default
// (https://learn.microsoft.com/en-us/defender-endpoint/api/api-hello-world).

const (
	defaultAPIURL       = "https://api.security.microsoft.com"
	defaultAuthorityURL = "https://login.microsoftonline.com"
	defaultScope        = "https://api.securitycenter.microsoft.com/.default"
	commentPrefix       = "Zero Root: "
	maxComment          = 200
)

// machineID is the id of a device in Defender for Endpoint: 40 hex characters.
var machineID = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	tenant := env.Require("DEFENDER_TENANT_ID")
	clientID := env.Require("DEFENDER_CLIENT_ID")
	secret := env.Secret("DEFENDER_CLIENT_SECRET")
	api := env.OptionalURL("DEFENDER_API_URL", defaultAPIURL)
	authority := env.OptionalURL("DEFENDER_AUTHORITY_URL", defaultAuthorityURL)
	scope := env.Optional("DEFENDER_SCOPE", defaultScope)
	timeout := env.Duration("DEFENDER_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	cc, err := kit.MicrosoftClientCredentials(authority, tenant, clientID, secret, scope)
	if err != nil {
		return nil, fmt.Errorf("configure sign-in: %w", err)
	}
	cc.HTTPClient = hc
	svc := &service{client: kit.NewClient(api, kit.ClientOptions{Auth: cc.Authenticator(), Logger: log, Timeout: timeout, HTTPClient: hc})}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "isolate_device",
		Description: "Response action. Isolate one Defender for Endpoint device from the network. Only the Defender " +
			"service and a few allowed destinations stay reachable. The request creates a machine action that the " +
			"device runs when it is online. Use isolation_type Full for a managed device. " +
			"Undo: call release_device with the same device_id.",
		Annotations: kit.Reversible("Isolate Defender device", true),
	}, v.isolate)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "release_device",
		Description: "Response action. Release one Defender for Endpoint device from isolation. " +
			"This is the undo of isolate_device. A second release while one is pending gives HTTP 400.",
		Annotations: kit.Reversible("Release Defender device", true),
	}, v.release)
}

type isolateInput struct {
	DeviceID      string `json:"device_id" jsonschema:"Defender device id: 40 hexadecimal characters"`
	IsolationType string `json:"isolation_type,omitempty" jsonschema:"Full, Selective or UnManagedDevice. The default is Full."`
	Comment       string `json:"comment,omitempty" jsonschema:"reason for the action, up to 200 characters. It is stored with the machine action."`
}

type releaseInput struct {
	DeviceID string `json:"device_id" jsonschema:"Defender device id: 40 hexadecimal characters"`
	Comment  string `json:"comment,omitempty" jsonschema:"reason for the action, up to 200 characters. It is stored with the machine action."`
}

type actionOutput struct {
	DeviceID string `json:"device_id"`
	ActionID string `json:"action_id"`
	Type     string `json:"type"`
	Status   string `json:"status"`
}

type machineAction struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	MachineID string `json:"machineId"`
}

func (v *service) isolate(ctx context.Context, _ *mcp.CallToolRequest, in isolateInput) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{DeviceID: in.DeviceID}
	if in.IsolationType == "" {
		in.IsolationType = "Full"
	}
	switch in.IsolationType {
	case "Full", "Selective", "UnManagedDevice":
	default:
		return nil, out, fmt.Errorf("%w: isolation_type must be Full, Selective or UnManagedDevice", kit.ErrInvalidInput)
	}
	return v.act(ctx, "isolate", in.DeviceID, in.Comment, in.IsolationType)
}

func (v *service) release(ctx context.Context, _ *mcp.CallToolRequest, in releaseInput) (*mcp.CallToolResult, actionOutput, error) {
	return v.act(ctx, "unisolate", in.DeviceID, in.Comment, "")
}

func (v *service) act(ctx context.Context, op, device, comment, isolationType string) (*mcp.CallToolResult, actionOutput, error) {
	out := actionOutput{DeviceID: device}
	if err := kit.CheckString("device_id", device, 40, machineID); err != nil {
		return nil, out, err
	}
	if comment == "" {
		comment = "response action"
	}
	if err := kit.CheckString("comment", comment, maxComment, nil); err != nil {
		return nil, out, err
	}
	body := map[string]string{"Comment": commentPrefix + comment}
	if isolationType != "" {
		body["IsolationType"] = isolationType
	}
	var ma machineAction
	err := v.client.Do(ctx, kit.Request{Method: http.MethodPost, Path: kit.Path("api", "machines", device, op), JSON: body}, &ma)
	if err != nil {
		return nil, out, defenderError(op, err)
	}
	wantType := map[string]string{"isolate": "Isolate", "unisolate": "Unisolate"}[op]
	if ma.ID == "" || ma.Type != wantType || !strings.EqualFold(ma.MachineID, device) {
		return nil, out, fmt.Errorf("%s: Defender answered with a machine action that does not match the request", op)
	}
	out.ActionID, out.Type, out.Status = ma.ID, ma.Type, ma.Status
	return nil, out, nil
}

// defenderError shows the error message of Defender, which has the shape
// {"error": {"code": "...", "message": "..."}}.
func defenderError(op string, err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if kit.DecodeErrorBody(ae, &body) == nil && body.Error.Message != "" {
			return fmt.Errorf("%s: Defender answered HTTP %d, %s: %s", op, ae.Status, body.Error.Code, body.Error.Message)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
