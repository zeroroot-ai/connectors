package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

const (
	devID   = "fb95f07d-9e73-411d-99ab-7eca3a5122b1"
	entryID = "d0c03fbb-43b9-4dff-840b-974ef227384d"
	depID   = "b5171742-1742-b517-4217-17b5421717b5"
	audID   = "7f960f66-b6ed-6d54-f24a-9b1021a1d17f"
)

// The mock follows the beta reference of the Windows Update deployment
// service (https://learn.microsoft.com/en-us/graph/api/resources/windowsupdates-deployment?view=graph-rest-beta):
// create answers 201, update and delete answer 202, updateAudienceById
// answers 202 with no body. The answers come from the files in testdata.
type mock struct {
	calls        []string
	bodies       []string
	auth         []string
	tokenForm    map[string]string
	deployment   string // body of the deployment answer
	members      string // body of the members answer
	failAudience bool
	deleted      bool
}

var requestedRe = regexp.MustCompile(`"requestedValue":"[a-z]+"`)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/contoso.onmicrosoft.com/oauth2/v2.0/token" {
		_ = r.ParseForm()
		m.tokenForm = map[string]string{}
		for k := range r.PostForm {
			m.tokenForm[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3599,"access_token":"graph-token"}`))
		return
	}
	b, _ := io.ReadAll(r.Body)
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	m.bodies = append(m.bodies, string(b))
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	base := "/beta/admin/windows/updates/"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base+"catalog/entries":
		_, _ = w.Write([]byte(`{"value":[` +
			`{"@odata.type":"#microsoft.graph.windowsUpdates.featureUpdateCatalogEntry","id":"c1dec151-c151-c1de-51c1-dec151c1dec1","displayName":"Win 11","version":"24H2"},` +
			`{"@odata.type":"#microsoft.graph.windowsUpdates.qualityUpdateCatalogEntry","id":"` + entryID + `","displayName":"2026.11 B","catalogName":"cu","qualityUpdateClassification":"security"}]}`))
	case r.Method == http.MethodPost && r.URL.Path == base+"deployments":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(m.deployment))
	case r.Method == http.MethodPost && r.URL.Path == base+"deployments/"+depID+"/audience/updateAudienceById":
		if m.failAudience {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"BadRequest","message":"device not found"}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet && r.URL.Path == base+"deployments/"+depID+"/audience":
		_, _ = w.Write([]byte(`{"@odata.type":"#microsoft.graph.windowsUpdates.deploymentAudience","id":"` + audID + `"}`))
	case r.Method == http.MethodGet && r.URL.Path == base+"deploymentAudiences/"+audID+"/members":
		_, _ = w.Write([]byte(m.members))
	case r.Method == http.MethodGet && r.URL.Path == base+"deployments/"+depID:
		_, _ = w.Write([]byte(m.deployment))
	case r.Method == http.MethodPatch && r.URL.Path == base+"deployments/"+depID:
		var body struct {
			State struct {
				RequestedValue string `json:"requestedValue"`
			} `json:"state"`
		}
		_ = json.Unmarshal(b, &body)
		m.deployment = requestedRe.ReplaceAllString(m.deployment, `"requestedValue":"`+body.State.RequestedValue+`"`)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(m.deployment))
	case r.Method == http.MethodDelete && r.URL.Path == base+"deployments/"+depID:
		m.deleted = true
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"Request_ResourceNotFound","message":"Resource does not exist."}}`))
	}
}

func setup(t *testing.T) (srv *mock, call func(string, map[string]any) (bool, string, *mcp.CallToolResult), tools map[string]*mcp.Tool) {
	t.Helper()
	m := &mock{deployment: fixture(t, "deployment_ok.json"), members: fixture(t, "members_one.json")}
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	register, err := buildWith(kit.NewEnvFrom(map[string]string{
		"INTUNE_TENANT_ID": "contoso.onmicrosoft.com", "INTUNE_CLIENT_ID": "app-id", "INTUNE_CLIENT_SECRET": "s3cret",
		"INTUNE_GRAPH_URL": ts.URL, "INTUNE_AUTHORITY_URL": ts.URL,
	}), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := kittest.Connect(t, register)
	return m, func(tool string, args map[string]any) (bool, string, *mcp.CallToolResult) {
		res := kittest.Call(t, cs, tool, args)
		return res.IsError, kittest.Text(t, res), res
	}, kittest.Tools(t, cs)
}

// sameJSON fails the test unless the body equals the golden file. A change of
// a request body then shows in the diff of the pull request.
func sameJSON(t *testing.T, got, golden string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(fixture(t, golden)), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("body differs from testdata/%s:\n got  %s\n want %s", golden, got, fixture(t, golden))
	}
}

func TestStartPatchDeployment(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality"})
	if isErr {
		t.Fatal(text)
	}
	var out deploymentOutput
	kittest.Structured(t, res, &out)
	if out.DeploymentID != depID || out.DeviceID != devID || out.State != "offering" {
		t.Errorf("out = %+v", out)
	}
	want := []string{
		"POST /beta/admin/windows/updates/deployments",
		"POST /beta/admin/windows/updates/deployments/" + depID + "/audience/updateAudienceById",
	}
	if strings.Join(m.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v", m.calls)
	}
	sameJSON(t, m.bodies[0], "create_deployment.json")
	sameJSON(t, m.bodies[1], "update_audience.json")
	for _, a := range m.auth {
		if a != "Bearer graph-token" {
			t.Errorf("auth = %q", a)
		}
	}
	if f := m.tokenForm; f["client_id"] != "app-id" || f["scope"] != "https://graph.microsoft.com/.default" {
		t.Errorf("token form = %v", f)
	}
}

func TestStartDeletesTheDeploymentWhenTheAudienceFails(t *testing.T) {
	m, call, _ := setup(t)
	m.failAudience = true
	isErr, text, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality"})
	if !isErr || !strings.Contains(text, "device not found") || !m.deleted {
		t.Errorf("isErr=%v deleted=%v %s", isErr, m.deleted, text)
	}
}

func TestGetPatchDeployment(t *testing.T) {
	_, call, _ := setup(t)
	isErr, text, res := call("get_patch_deployment", map[string]any{"deployment_id": depID})
	if isErr {
		t.Fatal(text)
	}
	var out deploymentOutput
	kittest.Structured(t, res, &out)
	if out.State != "offering" || out.Device == nil || out.Device.Enrollment["quality"].EnrollmentState != "enrolled" {
		t.Errorf("out = %+v", out)
	}
}

func TestRollbackAndResume(t *testing.T) {
	m, call, _ := setup(t)
	isErr, text, res := call("rollback_patch_deployment", map[string]any{"deployment_id": depID})
	if isErr {
		t.Fatal(text)
	}
	var out deploymentOutput
	kittest.Structured(t, res, &out)
	if out.Requested != "paused" {
		t.Errorf("out = %+v", out)
	}
	sameJSON(t, m.bodies[len(m.bodies)-1], "pause_deployment.json")
	if isErr, text, _ := call("resume_patch_deployment", map[string]any{"deployment_id": depID}); isErr {
		t.Fatal(text)
	}
	if !strings.Contains(m.deployment, `"requestedValue":"none"`) {
		t.Errorf("deployment = %s", m.deployment)
	}
}

func TestRemovePatchDeployment(t *testing.T) {
	m, call, _ := setup(t)
	if isErr, text, _ := call("remove_patch_deployment", map[string]any{"deployment_id": depID}); isErr || !m.deleted {
		t.Fatalf("isErr=%v deleted=%v %s", isErr, m.deleted, text)
	}
}

// TestRefusesAudienceOfManyDevices makes sure that no tool changes a
// deployment that holds more than one device.
func TestRefusesAudienceOfManyDevices(t *testing.T) {
	m, call, _ := setup(t)
	m.members = fixture(t, "members_two.json")
	for _, tool := range []string{"rollback_patch_deployment", "resume_patch_deployment", "remove_patch_deployment", "get_patch_deployment"} {
		if isErr, text, _ := call(tool, map[string]any{"deployment_id": depID}); !isErr || !strings.Contains(text, "exactly one device") {
			t.Errorf("%s: isErr=%v %s", tool, isErr, text)
		}
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "PATCH") || strings.HasPrefix(c, "DELETE") {
			t.Errorf("a change reached Graph: %s", c)
		}
	}
}

// TestShapeChange makes sure that a change of the beta answer stops the tool.
func TestShapeChange(t *testing.T) {
	m, call, _ := setup(t)
	m.deployment = fixture(t, "deployment_changed_shape.json")
	if isErr, text, _ := call("get_patch_deployment", map[string]any{"deployment_id": depID}); !isErr || !strings.Contains(text, "new shape") {
		t.Errorf("get: isErr=%v %s", isErr, text)
	}
	if isErr, text, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality"}); !isErr || !strings.Contains(text, "new shape") {
		t.Errorf("start: isErr=%v %s", isErr, text)
	}
	for _, c := range m.calls {
		if strings.Contains(c, "updateAudienceById") {
			t.Errorf("the tool went on after a shape change: %s", c)
		}
	}
}

func TestPinnedToBeta(t *testing.T) {
	if graphVersion != "beta" {
		t.Fatalf("graphVersion = %q", graphVersion)
	}
	m, call, _ := setup(t)
	call("get_patch_deployment", map[string]any{"deployment_id": depID})
	for _, c := range m.calls {
		if !strings.Contains(c, "/beta/") {
			t.Errorf("call outside the beta path: %s", c)
		}
	}
}

func TestListCatalogEntries(t *testing.T) {
	_, call, _ := setup(t)
	_, text, res := call("list_catalog_entries", map[string]any{"kind": "quality"})
	var out catalogOutput
	kittest.Structured(t, res, &out)
	if len(out.Entries) != 1 || out.Entries[0].ID != entryID || strings.Contains(text, "Win 11") {
		t.Errorf("out = %+v", out)
	}
	for _, in := range []map[string]any{{"kind": "driver"}, {}, {"kind": "quality", "limit": 51}} {
		if isErr, _, _ := call("list_catalog_entries", in); !isErr {
			t.Errorf("accepted %v", in)
		}
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	bad := []string{"", "x", depID + "/audience", "../" + depID, depID + "?a=1", depID + "x", strings.Repeat("a", 100)}
	for _, b := range bad {
		for _, tool := range []string{"get_patch_deployment", "rollback_patch_deployment", "resume_patch_deployment", "remove_patch_deployment"} {
			if isErr, _, _ := call(tool, map[string]any{"deployment_id": b}); !isErr {
				t.Errorf("%s accepted %q", tool, b)
			}
		}
		if isErr, _, _ := call("start_patch_deployment", map[string]any{"entra_device_id": b, "catalog_entry_id": entryID, "kind": "quality"}); !isErr {
			t.Errorf("start accepted device %q", b)
		}
		if isErr, _, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": b, "kind": "quality"}); !isErr {
			t.Errorf("start accepted entry %q", b)
		}
	}
	if isErr, _, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "driver"}); !isErr {
		t.Error("start accepted kind driver")
	}
	if len(m.calls) != 0 {
		t.Errorf("an invalid input reached Graph: %v", m.calls)
	}
}

func TestAnnotationsAndReadOnly(t *testing.T) {
	m, call, tools := setup(t)
	if len(tools) != 6 {
		t.Fatalf("tools = %d", len(tools))
	}
	kittest.AssertReversible(t, tools, "start_patch_deployment", "rollback_patch_deployment")
	kittest.AssertReversible(t, tools, "rollback_patch_deployment", "resume_patch_deployment")
	kittest.AssertReversible(t, tools, "resume_patch_deployment", "rollback_patch_deployment")
	kittest.AssertIrreversible(t, tools, "remove_patch_deployment")
	kittest.AssertIdempotent(t, tools, "start_patch_deployment", false)
	kittest.AssertIdempotent(t, tools, "rollback_patch_deployment", true)
	for _, name := range []string{"list_catalog_entries", "get_patch_deployment"} {
		if tl := tools[name]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read only", name)
		}
	}
	call("list_catalog_entries", map[string]any{"kind": "quality"})
	call("get_patch_deployment", map[string]any{"deployment_id": depID})
	for _, c := range m.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("a read tool sent %s", c)
		}
	}
}

func TestConfig(t *testing.T) {
	if _, err := build(kit.NewEnvFrom(nil), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("empty environment accepted")
	}
}
