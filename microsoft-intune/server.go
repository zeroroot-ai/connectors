package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools use the Windows Update deployment service in the beta API of
// Microsoft Graph. Graph v1.0 has no call that patches one device. The owner
// chose the beta API on 2026-11-21. The API reference is in the repo
// microsoftgraph/microsoft-graph-docs-contrib, folder api-reference/beta, and
// on https://learn.microsoft.com/en-us/graph/api/resources/windowsupdates-deployment?view=graph-rest-beta
//   - GET    /admin/windows/updates/catalog/entries        (list entries)
//   - POST   /admin/windows/updates/deployments            (create deployment, 201)
//   - GET    /admin/windows/updates/deployments/{id}       (get deployment, 200)
//   - PATCH  /admin/windows/updates/deployments/{id}       (update deployment, 202)
//   - DELETE /admin/windows/updates/deployments/{id}       (delete deployment, 202)
//   - GET    /admin/windows/updates/deployments/{id}/audience           (the audience of a deployment)
//   - POST   /admin/windows/updates/deployments/{id}/audience/updateAudienceById (202)
//   - GET    /admin/windows/updates/deploymentAudiences/{audienceId}/members (list members)
// Permission: WindowsUpdates.ReadWrite.All to change, WindowsUpdates.Read.All to read.
//
// The version is pinned in two places. graphVersion is the path of the API.
// The shape of each answer is checked on each call (see checkDeployment), so
// a change of the beta API stops the tool with a clear error. The test
// TestShapeChange and the golden files in testdata detect the same change.

// catalogPage is the number of catalog entries that list_catalog_entries reads.
const catalogPage = 100

const (
	graphVersion        = "beta"
	defaultGraphURL     = "https://graph.microsoft.com"
	defaultAuthorityURL = "https://login.microsoftonline.com"
	graphScope          = "https://graph.microsoft.com/.default"

	typeDeployment     = "#microsoft.graph.windowsUpdates.deployment"
	typeCatalogContent = "#microsoft.graph.windowsUpdates.catalogContent"
	typeDeployState    = "microsoft.graph.windowsUpdates.deploymentState"
	typeAzureADDevice  = "#microsoft.graph.windowsUpdates.azureADDevice"
)

// entryTypes maps the input kind of an update to the type of its catalog entry.
var entryTypes = map[string]string{
	"quality": "#microsoft.graph.windowsUpdates.qualityUpdateCatalogEntry",
	"feature": "#microsoft.graph.windowsUpdates.featureUpdateCatalogEntry",
}

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	tenant := env.Require("INTUNE_TENANT_ID")
	clientID := env.Require("INTUNE_CLIENT_ID")
	secret := env.Secret("INTUNE_CLIENT_SECRET")
	graph := env.OptionalURL("INTUNE_GRAPH_URL", defaultGraphURL)
	authority := env.OptionalURL("INTUNE_AUTHORITY_URL", defaultAuthorityURL)
	timeout := env.Duration("INTUNE_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	cc, err := kit.MicrosoftClientCredentials(authority, tenant, clientID, secret, graphScope)
	if err != nil {
		return nil, fmt.Errorf("configure sign-in: %w", err)
	}
	cc.HTTPClient = hc
	base := *graph
	base.Path += "/" + graphVersion
	svc := &service{client: kit.NewClient(&base, kit.ClientOptions{Auth: cc.Authenticator(), Logger: log, Timeout: timeout, HTTPClient: hc})}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_catalog_entries",
		Description: "Read the Windows Update catalog: the updates that start_patch_deployment can deploy. Read only. " +
			"Give kind quality (monthly cumulative updates) or feature (Windows versions). " +
			"Returns at most limit entries, newest first.",
		Annotations: kit.ReadOnly("List Windows Update catalog entries"),
	}, v.listCatalog)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "start_patch_deployment",
		Description: "Patch action. Start a Windows Update deployment of one catalog update for exactly one device. " +
			"The audience of the deployment holds that device and no other. Windows Update offers the update to " +
			"the device. The device installs it when it applies the offer. " +
			"No tool undoes this action: an installed update stays installed. " +
			"rollback_patch_deployment stops the offer before the device applies it, and it does not uninstall anything. " +
			"Read the result with get_patch_deployment.",
		Annotations: kit.Irreversible("Start Windows patch deployment for one device", false),
	}, v.start)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_patch_deployment",
		Description: "Read one Windows Update deployment: state, reasons, and the state and errors of its device. " +
			"Read only. A deployment that Intune created, or one with more than one device, can be refused.",
		Annotations: kit.ReadOnly("Get Windows patch deployment"),
	}, v.get)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "rollback_patch_deployment",
		Description: "Patch action. Pause one Windows Update deployment, so Windows Update stops the offer to its device. " +
			"It does not uninstall an update that the device already installed. The deployment must hold exactly one device. " +
			"Undo: call resume_patch_deployment with the same deployment_id.",
		Annotations: kit.Reversible("Pause Windows patch deployment", true),
	}, v.rollback)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "resume_patch_deployment",
		Description: "Patch action. Resume one paused Windows Update deployment. The deployment must hold exactly one device. " +
			"This is the undo of rollback_patch_deployment.",
		Annotations: kit.Reversible("Resume Windows patch deployment", true),
	}, v.resume)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "remove_patch_deployment",
		Description: "Patch action. Delete one Windows Update deployment. The offer to its device stops. The deployment must " +
			"hold exactly one device. No tool undoes this action: Graph cannot restore a deleted deployment. " +
			"To stop the offer and keep the deployment, call rollback_patch_deployment.",
		Annotations: kit.Irreversible("Delete Windows patch deployment", true),
	}, v.remove)
}

// ---- shapes -------------------------------------------------------------

// deployment is the part of a Graph deployment that the tools read.
type deployment struct {
	OdataType string `json:"@odata.type"`
	ID        string `json:"id"`
	State     struct {
		EffectiveValue string `json:"effectiveValue"`
		Value          string `json:"value"`
		RequestedValue string `json:"requestedValue"`
		Reasons        []struct {
			Value string `json:"value"`
		} `json:"reasons"`
	} `json:"state"`
	CreatedDateTime      string `json:"createdDateTime"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
}

// errShape marks an answer of Graph that does not have the shape that the
// reference of the beta API states.
var errShape = errors.New("the Graph beta answer has a new shape")

// checkDeployment makes sure that an answer has the fields that the tools
// need. The beta API can change without a notice. A change stops the tool
// here, and the tool does not act on a field that it does not understand.
func checkDeployment(d *deployment) error {
	switch {
	case d.OdataType != "" && d.OdataType != typeDeployment:
		return fmt.Errorf("%w: @odata.type is %q", errShape, d.OdataType)
	case kit.CheckString("id", d.ID, 36, kit.UUID) != nil:
		return fmt.Errorf("%w: id is missing or is not a GUID", errShape)
	case d.State.RequestedValue == "":
		return fmt.Errorf("%w: state.requestedValue is missing", errShape)
	}
	return nil
}

func (d *deployment) effective() string {
	if d.State.EffectiveValue != "" {
		return d.State.EffectiveValue
	}
	return d.State.Value
}

type deviceState struct {
	ID         string           `json:"id"`
	Errors     []map[string]any `json:"errors,omitempty"`
	Enrollment map[string]struct {
		EnrollmentState string `json:"enrollmentState"`
	} `json:"enrollment,omitempty"`
}

type membersAnswer struct {
	Value    []deviceState `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
}

// ---- tools --------------------------------------------------------------

type catalogInput struct {
	Kind  string `json:"kind" jsonschema:"quality or feature"`
	Limit int    `json:"limit,omitempty" jsonschema:"number of entries, 1 to 50. The default is 20."`
}

type catalogEntry struct {
	ID                        string `json:"id"`
	DisplayName               string `json:"displayName,omitempty"`
	ReleaseDateTime           string `json:"releaseDateTime,omitempty"`
	DeployableUntilDateTime   string `json:"deployableUntilDateTime,omitempty"`
	CatalogName               string `json:"catalogName,omitempty"`
	QualityUpdateClass        string `json:"qualityUpdateClassification,omitempty"`
	Version                   string `json:"version,omitempty"`
	IsExpeditable             bool   `json:"isExpeditable,omitempty"`
	QualityUpdateCadenceValue string `json:"qualityUpdateCadence,omitempty"`
}

type catalogOutput struct {
	Entries []catalogEntry `json:"entries"`
}

func (v *service) listCatalog(ctx context.Context, _ *mcp.CallToolRequest, in catalogInput) (*mcp.CallToolResult, catalogOutput, error) {
	out := catalogOutput{Entries: []catalogEntry{}}
	typ, ok := entryTypes[in.Kind]
	if !ok {
		return nil, out, fmt.Errorf("%w: kind must be quality or feature", kit.ErrInvalidInput)
	}
	limit := in.Limit
	if limit == 0 {
		limit = 20
	}
	if err := kit.CheckRange("limit", limit, 1, 50); err != nil {
		return nil, out, fmt.Errorf("check limit: %w", err)
	}
	q := url.Values{}
	// The reference documents $orderby and $top. The tool reads the newest
	// page and keeps the entries of the kind that the caller asked for.
	q.Set("$orderby", "releaseDateTime desc")
	q.Set("$top", strconv.Itoa(catalogPage))
	var resp struct {
		Value []struct {
			OdataType string `json:"@odata.type"`
			catalogEntry
		} `json:"value"`
	}
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("admin", "windows", "updates", "catalog", "entries"), Query: q}, &resp)
	if err != nil {
		return nil, out, kit.GraphError("list catalog entries", err)
	}
	for _, e := range resp.Value {
		if e.OdataType == typ && len(out.Entries) < limit {
			out.Entries = append(out.Entries, e.catalogEntry)
		}
	}
	return nil, out, nil
}

type startInput struct {
	DeviceID       string `json:"entra_device_id" jsonschema:"Microsoft Entra device id of the one device: a GUID. It is the device id and not the object id."`
	CatalogEntryID string `json:"catalog_entry_id" jsonschema:"id of the update, from list_catalog_entries: a GUID"`
	Kind           string `json:"kind" jsonschema:"quality or feature. It must match the kind of the catalog entry."`
}

type deploymentOutput struct {
	DeploymentID string       `json:"deployment_id"`
	DeviceID     string       `json:"entra_device_id,omitempty"`
	Action       string       `json:"action"`
	State        string       `json:"state,omitempty"`
	Requested    string       `json:"requested_state,omitempty"`
	Reasons      []string     `json:"reasons,omitempty"`
	Created      string       `json:"created,omitempty"`
	Modified     string       `json:"last_modified,omitempty"`
	Device       *deviceState `json:"device,omitempty"`
}

func (o *deploymentOutput) fill(d *deployment) {
	o.DeploymentID, o.State, o.Requested = d.ID, d.effective(), d.State.RequestedValue
	o.Created, o.Modified = d.CreatedDateTime, d.LastModifiedDateTime
	for _, r := range d.State.Reasons {
		o.Reasons = append(o.Reasons, r.Value)
	}
}

// createBody is the body of the create call. The audience stays empty until
// the second call adds the one device. An empty audience gets no offer.
func createBody(typ, entryID string) map[string]any {
	return map[string]any{
		"@odata.type": typeDeployment,
		"content": map[string]any{
			"@odata.type":  typeCatalogContent,
			"catalogEntry": map[string]any{"@odata.type": typ, "id": entryID},
		},
	}
}

func audienceBody(deviceID string) map[string]any {
	return map[string]any{"memberEntityType": typeAzureADDevice, "addMembers": []string{deviceID}}
}

func stateBody(requested string) map[string]any {
	return map[string]any{
		"@odata.type": typeDeployment,
		"state":       map[string]any{"@odata.type": typeDeployState, "requestedValue": requested},
	}
}

func deploymentPath(id string, more ...string) string {
	return kit.Path(append([]string{"admin", "windows", "updates", "deployments", id}, more...)...)
}

func (v *service) start(ctx context.Context, _ *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, deploymentOutput, error) {
	out := deploymentOutput{DeviceID: in.DeviceID, Action: "start_patch_deployment"}
	typ, ok := entryTypes[in.Kind]
	if !ok {
		return nil, out, fmt.Errorf("%w: kind must be quality or feature", kit.ErrInvalidInput)
	}
	if err := kit.CheckString("entra_device_id", in.DeviceID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	if err := kit.CheckString("catalog_entry_id", in.CatalogEntryID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	var d deployment
	// A create call is not repeated: a repeat would make a second deployment.
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("admin", "windows", "updates", "deployments"),
		JSON: createBody(typ, in.CatalogEntryID),
	}, &d)
	if err != nil {
		return nil, out, kit.GraphError("create deployment", err)
	}
	if err := checkDeployment(&d); err != nil {
		return nil, out, fmt.Errorf("create deployment: %w", err)
	}
	out.fill(&d)
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: deploymentPath(d.ID, "audience", "updateAudienceById"),
		JSON: audienceBody(in.DeviceID), Retry: true, // adding the same member twice changes nothing
	}, nil)
	if err != nil {
		// The audience is empty, so the deployment offers nothing. Delete it
		// so that no empty deployment stays.
		if delErr := v.client.Do(ctx, kit.Request{Method: http.MethodDelete, Path: deploymentPath(d.ID)}, nil); delErr != nil {
			return nil, out, fmt.Errorf("%w: the deployment %s stays with an empty audience because its delete failed: %w",
				kit.GraphError("add the device to the audience", err), d.ID, delErr)
		}
		out.DeploymentID = ""
		return nil, out, kit.GraphError("add the device to the audience (the deployment was deleted)", err)
	}
	return nil, out, nil
}

type deploymentInput struct {
	DeploymentID string `json:"deployment_id" jsonschema:"id of the deployment: a GUID"`
}

// load reads a deployment and checks its shape.
func (v *service) load(ctx context.Context, id string) (*deployment, error) {
	var d deployment
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: deploymentPath(id)}, &d); err != nil {
		return nil, kit.GraphError("get deployment", err)
	}
	if err := checkDeployment(&d); err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	return &d, nil
}

// onlyDevice returns the one device in the audience of a deployment. It fails
// when the audience holds no device or more than one. A tool that changes a
// deployment calls it first, so the tools never act on a deployment of a
// group of devices.
func (v *service) onlyDevice(ctx context.Context, id string) (*deviceState, error) {
	var aud struct {
		ID string `json:"id"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: deploymentPath(id, "audience")}, &aud); err != nil {
		return nil, kit.GraphError("get audience", err)
	}
	if kit.CheckString("audience id", aud.ID, 36, kit.UUID) != nil {
		return nil, fmt.Errorf("get audience: %w: id is missing or is not a GUID", errShape)
	}
	var m membersAnswer
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("admin", "windows", "updates", "deploymentAudiences", aud.ID, "members")}, &m)
	if err != nil {
		return nil, kit.GraphError("list audience members", err)
	}
	if m.NextLink != "" || len(m.Value) != 1 {
		return nil, errors.New("refused: the audience of this deployment does not hold exactly one device")
	}
	if kit.CheckString("device id", m.Value[0].ID, 36, kit.UUID) != nil {
		return nil, fmt.Errorf("list audience members: %w: member id is missing or is not a GUID", errShape)
	}
	return &m.Value[0], nil
}

func (v *service) get(ctx context.Context, _ *mcp.CallToolRequest, in deploymentInput) (*mcp.CallToolResult, deploymentOutput, error) {
	out := deploymentOutput{DeploymentID: in.DeploymentID, Action: "get_patch_deployment"}
	if err := kit.CheckString("deployment_id", in.DeploymentID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	d, err := v.load(ctx, in.DeploymentID)
	if err != nil {
		return nil, out, err
	}
	dev, err := v.onlyDevice(ctx, in.DeploymentID)
	if err != nil {
		return nil, out, err
	}
	out.fill(d)
	out.Device, out.DeviceID = dev, dev.ID
	return nil, out, nil
}

func (v *service) rollback(ctx context.Context, _ *mcp.CallToolRequest, in deploymentInput) (*mcp.CallToolResult, deploymentOutput, error) {
	return v.setState(ctx, in, "paused", "rollback_patch_deployment")
}

func (v *service) resume(ctx context.Context, _ *mcp.CallToolRequest, in deploymentInput) (*mcp.CallToolResult, deploymentOutput, error) {
	return v.setState(ctx, in, "none", "resume_patch_deployment")
}

func (v *service) setState(ctx context.Context, in deploymentInput, requested, action string) (*mcp.CallToolResult, deploymentOutput, error) {
	out := deploymentOutput{DeploymentID: in.DeploymentID, Action: action}
	if err := kit.CheckString("deployment_id", in.DeploymentID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	dev, err := v.onlyDevice(ctx, in.DeploymentID)
	if err != nil {
		return nil, out, err
	}
	out.DeviceID = dev.ID
	var d deployment
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPatch, Path: deploymentPath(in.DeploymentID),
		JSON: stateBody(requested), Retry: true, // the same value twice has the same effect
	}, &d)
	if err != nil {
		return nil, out, kit.GraphError(action, err)
	}
	if err := checkDeployment(&d); err != nil {
		return nil, out, fmt.Errorf("%s: %w", action, err)
	}
	if d.State.RequestedValue != requested {
		return nil, out, fmt.Errorf("%s: Graph did not accept the requested state %q", action, requested)
	}
	out.fill(&d)
	return nil, out, nil
}

func (v *service) remove(ctx context.Context, _ *mcp.CallToolRequest, in deploymentInput) (*mcp.CallToolResult, deploymentOutput, error) {
	out := deploymentOutput{DeploymentID: in.DeploymentID, Action: "remove_patch_deployment"}
	if err := kit.CheckString("deployment_id", in.DeploymentID, 36, kit.UUID); err != nil {
		return nil, out, err
	}
	dev, err := v.onlyDevice(ctx, in.DeploymentID)
	if err != nil {
		return nil, out, err
	}
	out.DeviceID = dev.ID
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodDelete, Path: deploymentPath(in.DeploymentID)}, nil); err != nil {
		return nil, out, kit.GraphError("remove_patch_deployment", err)
	}
	out.State = "deleted"
	return nil, out, nil
}
