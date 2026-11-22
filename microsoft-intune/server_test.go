package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
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
	queries      []url.Values
	notExpedite  bool // the quality entry has isExpeditable false
	nextPage     bool // the catalog has a second page of quality entries
}

var requestedRe = regexp.MustCompile(`"requestedValue":"[a-z]+"`)

func fixture(t *testing.T, name string) string {
	t.Helper()
	root, err := os.OpenRoot("testdata")
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer func() { _ = root.Close() }()
	b, err := root.ReadFile(name)
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
	m.queries = append(m.queries, r.URL.Query())
	base := "/beta/admin/windows/updates/"
	const qualityType = "#microsoft.graph.windowsUpdates.qualityUpdateCatalogEntry"
	switch {
	case strings.HasPrefix(r.URL.Path, base+"catalog/entries"):
		m.serveEntries(w, r)
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
	isErr, text, res := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2})
	if isErr {
		t.Fatal(text)
	}
	var out deploymentOutput
	kittest.Structured(t, res, &out)
	if out.DeploymentID != depID || out.DeviceID != devID || out.State != "offering" {
		t.Errorf("out = %+v", out)
	}
	want := []string{
		"GET /beta/admin/windows/updates/catalog/entries/" + entryID,
		"POST /beta/admin/windows/updates/deployments",
		"POST /beta/admin/windows/updates/deployments/" + depID + "/audience/updateAudienceById",
	}
	if strings.Join(m.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v", m.calls)
	}
	sameJSON(t, m.bodies[1], "create_deployment.json")
	sameJSON(t, m.bodies[2], "update_audience.json")
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
	isErr, text, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2})
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
	isErr, text, res := call("pause_patch_deployment", map[string]any{"deployment_id": depID})
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
	for _, tool := range []string{"pause_patch_deployment", "resume_patch_deployment", "remove_patch_deployment", "get_patch_deployment"} {
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
	if isErr, text, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2}); !isErr || !strings.Contains(text, "new shape") {
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
	m, call, _ := setup(t)
	_, text, res := call("list_catalog_entries", map[string]any{"kind": "quality", "limit": 5})
	var out catalogOutput
	kittest.Structured(t, res, &out)
	if len(out.Entries) != 1 || out.Entries[0].ID != entryID || strings.Contains(text, "Win 11") || out.HasMore {
		t.Errorf("out = %+v", out)
	}
	// Graph picks the kind with $filter, and the tool sends $top for the limit.
	q := m.queries[0]
	if q.Get("$filter") != "isof('microsoft.graph.windowsUpdates.qualityUpdateCatalogEntry')" || q.Get("$top") != "5" || q.Get("$orderby") != "releaseDateTime desc" {
		t.Errorf("query = %v", q)
	}
	_, _, res = call("list_catalog_entries", map[string]any{"kind": "feature"})
	var f catalogOutput
	kittest.Structured(t, res, &f)
	if len(f.Entries) != 1 || f.Entries[0].Version != "24H2" || m.queries[1].Get("$filter") != "isof('microsoft.graph.windowsUpdates.featureUpdateCatalogEntry')" {
		t.Errorf("feature = %+v query %v", f, m.queries[1])
	}
	for _, in := range []map[string]any{{"kind": "driver"}, {}, {"kind": "quality", "limit": 51}} {
		if isErr, _, _ := call("list_catalog_entries", in); !isErr {
			t.Errorf("accepted %v", in)
		}
	}
}

// The tool follows @odata.nextLink up to a bound, takes only its paging
// parameter, and never uses the host of the link. has_more is true when more
// entries exist than the limit.
func TestListCatalogEntriesPaging(t *testing.T) {
	m, call, _ := setup(t)
	m.nextPage = true
	_, _, res := call("list_catalog_entries", map[string]any{"kind": "quality", "limit": 2})
	var out catalogOutput
	kittest.Structured(t, res, &out)
	if len(out.Entries) != 2 || out.HasMore {
		t.Fatalf("out = %+v", out)
	}
	if len(m.queries) != 2 || m.queries[1].Get("$skiptoken") != "tok2" || m.queries[1].Get("$filter") == "" {
		t.Errorf("queries = %v", m.queries)
	}
	// With limit 1, the second entry is more than the limit.
	m.queries = nil
	_, _, res = call("list_catalog_entries", map[string]any{"kind": "quality", "limit": 1})
	var one catalogOutput
	kittest.Structured(t, res, &one)
	if len(one.Entries) != 1 || !one.HasMore || len(m.queries) != 1 {
		t.Errorf("limit 1: %+v queries %v", one, m.queries)
	}
	for _, c := range m.calls {
		if strings.Contains(c, "graph.microsoft.com") {
			t.Errorf("the tool used the host of the link: %s", c)
		}
	}
	if _, err := pageParams("http://x/y?$skiptoken=a"); err == nil {
		t.Error("a link with http was accepted")
	}
	if _, err := pageParams("https://x/y?other=1"); err == nil {
		t.Error("a link with no paging parameter was accepted")
	}
}

// A member that is a group of devices counts as one member. The tools refuse it.
func TestRefusesAnAudienceMemberThatIsNotADevice(t *testing.T) {
	for _, fixture := range []string{"members_group.json", "members_notype.json"} {
		m, call, _ := setup(t)
		m.members = fixture2(t, fixture)
		for _, tool := range []string{"pause_patch_deployment", "resume_patch_deployment", "remove_patch_deployment", "get_patch_deployment"} {
			if isErr, text, _ := call(tool, map[string]any{"deployment_id": depID}); !isErr || !strings.Contains(text, "may hold more than one device") {
				t.Errorf("%s with %s: isErr=%v %s", tool, fixture, isErr, text)
			}
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "PATCH") || strings.HasPrefix(c, "DELETE") {
				t.Errorf("%s: a change reached Graph: %s", fixture, c)
			}
		}
	}
}

func fixture2(t *testing.T, name string) string { t.Helper(); return fixture(t, name) }

func TestRefusesADeploymentWithNoType(t *testing.T) {
	m, call, _ := setup(t)
	m.deployment = fixture(t, "deployment_no_type.json")
	if isErr, text, _ := call("get_patch_deployment", map[string]any{"deployment_id": depID}); !isErr || !strings.Contains(text, "new shape") {
		t.Errorf("isErr=%v %s", isErr, text)
	}
}

func TestStartFeatureUpdateHasNoSettings(t *testing.T) {
	m, call, _ := setup(t)
	const featureID = "c1dec151-c151-c1de-51c1-dec151c1dec1"
	if isErr, text, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": featureID, "kind": "feature"}); isErr {
		t.Fatal(text)
	}
	sameJSON(t, m.bodies[1], "create_deployment_feature.json")
}

// A quality update is an expedited deployment, and the entry must be expeditable.
// A catalog entry of another kind than the input says is refused before any write.
func TestStartChecksTheCatalogEntry(t *testing.T) {
	m, call, _ := setup(t)
	m.notExpedite = true
	args := map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2}
	if isErr, text, _ := call("start_patch_deployment", args); !isErr || !strings.Contains(text, "cannot be expedited") {
		t.Errorf("not expeditable: isErr=%v %s", isErr, text)
	}
	m.notExpedite = false
	if isErr, text, _ := call("start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "feature"}); !isErr || !strings.Contains(text, "needs") {
		t.Errorf("wrong kind: isErr=%v %s", isErr, text)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a write reached Graph: %s", c)
		}
	}
}

func TestConfigRefusesAGraphURLWithAPath(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	base := map[string]string{"INTUNE_TENANT_ID": "t.onmicrosoft.com", "INTUNE_CLIENT_ID": "c", "INTUNE_CLIENT_SECRET": "s"}
	for _, bad := range []string{"https://graph.microsoft.com/v1.0", "https://graph.microsoft.com/beta", "https://graph.microsoft.com/x/"} {
		env := map[string]string{"INTUNE_GRAPH_URL": bad}
		for k, v := range base {
			env[k] = v
		}
		if _, err := build(kit.NewEnvFrom(env), log); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, ok := range []string{"https://graph.microsoft.com", "https://graph.microsoft.com/", "https://graph.microsoft.us"} {
		env := map[string]string{"INTUNE_GRAPH_URL": ok}
		for k, v := range base {
			env[k] = v
		}
		if _, err := build(kit.NewEnvFrom(env), log); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
}

func TestInputValidation(t *testing.T) {
	m, call, _ := setup(t)
	bad := []string{"", "x", depID + "/audience", "../" + depID, depID + "?a=1", depID + "x", strings.Repeat("a", 100)}
	for _, b := range bad {
		for _, tool := range []string{"get_patch_deployment", "pause_patch_deployment", "resume_patch_deployment", "remove_patch_deployment"} {
			if isErr, _, _ := call(tool, map[string]any{"deployment_id": b}); !isErr {
				t.Errorf("%s accepted %q", tool, b)
			}
		}
		if isErr, _, _ := call("start_patch_deployment", map[string]any{"entra_device_id": b, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2}); !isErr {
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
	kittest.AssertIrreversible(t, tools, "start_patch_deployment")
	kittest.AssertReversible(t, tools, "pause_patch_deployment", "resume_patch_deployment")
	kittest.AssertReversible(t, tools, "resume_patch_deployment", "pause_patch_deployment")
	kittest.AssertIrreversible(t, tools, "remove_patch_deployment")
	kittest.AssertIdempotent(t, tools, "start_patch_deployment", false)
	kittest.AssertIdempotent(t, tools, "pause_patch_deployment", true)
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

// serveCatalog answers the list of catalog entries. The server filters by type,
// as Graph does with $filter=isof(...), and pages with $skiptoken.
func (m *mock) serveCatalog(w http.ResponseWriter, r *http.Request) {
	const qualityType = "#microsoft.graph.windowsUpdates.qualityUpdateCatalogEntry"
	filter := r.URL.Query().Get("$filter")
	quality := `{"@odata.type":"` + qualityType + `","id":"` + entryID + `","displayName":"2026.11 B","catalogName":"cu","qualityUpdateClassification":"security"}`
	feature := `{"@odata.type":"#microsoft.graph.windowsUpdates.featureUpdateCatalogEntry","id":"c1dec151-c151-c1de-51c1-dec151c1dec1","displayName":"Win 11","version":"24H2"}`
	switch {
	case strings.Contains(filter, "qualityUpdateCatalogEntry") && r.URL.Query().Get("$skiptoken") == "tok2":
		_, _ = w.Write([]byte(`{"value":[` + strings.Replace(quality, entryID, "0c1d2e3f-0000-4000-8000-0000000000aa", 1) + `]}`))
	case strings.Contains(filter, "qualityUpdateCatalogEntry") && m.nextPage:
		_, _ = w.Write([]byte(`{"value":[` + quality + `],"@odata.nextLink":"https://graph.microsoft.com/beta/admin/windows/updates/catalog/entries?$skiptoken=tok2&$top=1"}`))
	case strings.Contains(filter, "qualityUpdateCatalogEntry"):
		_, _ = w.Write([]byte(`{"value":[` + quality + `]}`))
	case strings.Contains(filter, "featureUpdateCatalogEntry"):
		_, _ = w.Write([]byte(`{"value":[` + feature + `]}`))
	default:
		_, _ = w.Write([]byte(`{"value":[` + feature + `,` + quality + `]}`))
	}
}

// serveEntries answers the catalog calls: the list, and one entry by id.
func (m *mock) serveEntries(w http.ResponseWriter, r *http.Request) {
	const base = "/beta/admin/windows/updates/"
	const qualityType = "#microsoft.graph.windowsUpdates.qualityUpdateCatalogEntry"
	switch {
	case r.URL.Path == base+"catalog/entries":
		m.serveCatalog(w, r)
	case r.URL.Path == base+"catalog/entries/"+entryID:
		_, _ = w.Write([]byte(`{"@odata.type":"` + qualityType + `","id":"` + entryID + `","isExpeditable":` + strconv.FormatBool(!m.notExpedite) + `}`))
	case r.URL.Path == base+"catalog/entries/c1dec151-c151-c1de-51c1-dec151c1dec1":
		_, _ = w.Write([]byte(`{"@odata.type":"#microsoft.graph.windowsUpdates.featureUpdateCatalogEntry","id":"c1dec151-c151-c1de-51c1-dec151c1dec1"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// The restart constraint of the mission: kind quality needs both inputs, a false
// restart_allowed is refused before any call, and days go to the user experience
// settings. Kind feature takes neither input.
func TestStartQualityNeedsTheRestartInputs(t *testing.T) {
	m, call, _ := setup(t)
	base := func() map[string]any {
		return map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality"}
	}
	for name, mutate := range map[string]func(map[string]any){
		"no inputs":          func(a map[string]any) {},
		"no days":            func(a map[string]any) { a["restart_allowed"] = true },
		"no restart_allowed": func(a map[string]any) { a["days_until_forced_reboot"] = 2 },
		"days too high":      func(a map[string]any) { a["restart_allowed"], a["days_until_forced_reboot"] = true, 8 },
		"days negative":      func(a map[string]any) { a["restart_allowed"], a["days_until_forced_reboot"] = true, -1 },
	} {
		a := base()
		mutate(a)
		if isErr, _, _ := call("start_patch_deployment", a); !isErr {
			t.Errorf("%s: accepted", name)
		}
	}
	// restart_allowed false is refused with a clear reason, and nothing is written.
	a := base()
	a["restart_allowed"], a["days_until_forced_reboot"] = false, 2
	isErr, text, _ := call("start_patch_deployment", a)
	if !isErr || !strings.Contains(text, "does not allow a restart") || !strings.Contains(text, "Nothing was deployed") {
		t.Errorf("restart_allowed false: isErr=%v %s", isErr, text)
	}
	feature := map[string]any{"entra_device_id": devID, "catalog_entry_id": "c1dec151-c151-c1de-51c1-dec151c1dec1", "kind": "feature", "restart_allowed": true}
	if isErr, _, _ := call("start_patch_deployment", feature); !isErr {
		t.Error("kind feature accepted a restart input")
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a write reached Graph: %s", c)
		}
	}
	// With restart allowed, 0 and 7 days pass and go in the settings.
	for _, days := range []int{0, 7} {
		m.calls, m.bodies = nil, nil
		a := base()
		a["restart_allowed"], a["days_until_forced_reboot"] = true, days
		if isErr, text, _ := call("start_patch_deployment", a); isErr {
			t.Fatalf("days %d: %s", days, text)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(m.bodies[1]), &body)
		ux := body["settings"].(map[string]any)["userExperience"].(map[string]any)
		if ux["daysUntilForcedReboot"] != float64(days) {
			t.Errorf("days %d: userExperience = %v", days, ux)
		}
	}
}
