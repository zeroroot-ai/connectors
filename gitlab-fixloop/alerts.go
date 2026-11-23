package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// list_dependency_alerts uses two GitLab APIs.
//
// The vulnerabilities come from the GraphQL field Project.vulnerabilities:
//
//	POST /api/graphql
//	project(fullPath:) { vulnerabilities(reportType: [DEPENDENCY_SCANNING], state: [VulnerabilityState!],
//	  first: 100, after:) { pageInfo { hasNextPage endCursor } nodes { id state severity title webUrl
//	  identifiers { externalType externalId name url }
//	  location { ... on VulnerabilityLocationDependencyScanning { file dependency { package { name } version } } } } } }
//
// https://docs.gitlab.com/api/graphql/reference/ (the field Project.vulnerabilities, the object Vulnerability,
// VulnerabilityIdentifier, VulnerabilityLocationDependencyScanning and VulnerableDependency, and the enum
// VulnerabilityState: DETECTED, CONFIRMED, DISMISSED, RESOLVED). DETECTED and CONFIRMED map to open, RESOLVED to
// fixed and DISMISSED to dismissed. The GraphQL API needs GitLab Ultimate and the read_api scope.
//
// The dependency list gives the package manager, which the vulnerability has not:
//
//	GET /projects/:id/dependencies?per_page=100&page=N
//
// https://docs.gitlab.com/api/dependencies/ . Each entry has name, version, package_manager,
// dependency_file_path and vulnerabilities. GitLab leaves out the vulnerabilities key when the token cannot read the
// Project Security Dashboard, so the tool refuses a list where an entry lacks the key. An empty list means that no
// dependency scan ran. The tool refuses that too, because it cannot tell it from a clean project.
// A resolved vulnerability is no longer in the dependency list, so the tool takes its ecosystem from the name of its
// file (go.mod, package-lock.json and so on).
//
// The older GET /projects/:id/vulnerability_findings (https://docs.gitlab.com/api/vulnerability_findings/) is
// deprecated, so the tool does not use it.

const maxGraphErrors = 3

func (v *service) registerAlerts(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_dependency_alerts",
		Description: "List the dependency scanning vulnerabilities of a GitLab project. Read only. It needs GitLab Ultimate. " +
			"Each alert has a stable id, the state (open, fixed or dismissed), the package, the ecosystem, the installed version " +
			"and its package URL, the advisory ids (GHSA and CVE) from the identifiers of the vulnerability, the severity, the manifest path and the URL. " +
			"GitLab gives no vulnerable range and no patched version, so those fields are empty. " +
			"An alert with no advisory id has verifiable false: a caller cannot match it to a finding by advisory. " +
			"The state filter is open (the default), fixed, dismissed or all. " +
			"A token that cannot read the Security Dashboard, a project with no dependency scan and a GraphQL error are errors and never an empty list. " +
			"To verify a fix, call it with state all and treat an alert as fixed only when its state is fixed. " +
			"An alert that is missing from the list is not fixed. A truncated list proves nothing. " +
			"The tool reads at most 1000 vulnerabilities. When there are more, truncated is true. " +
			"A ref that is not the default branch is refused.",
		Annotations: kit.ReadOnly("List GitLab dependency alerts"),
	}, v.listDependencyAlerts)
}

type glDependency struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	PackageManager  string `json:"package_manager"`
	FilePath        string `json:"dependency_file_path"`
	Vulnerabilities *[]struct {
		ID json.Number `json:"id"`
	} `json:"vulnerabilities"`
}

func (v *service) listDependencyAlerts(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.AlertsInput) (*mcp.CallToolResult, fixloop.AlertsResult, error) {
	out := fixloop.AlertsResult{Project: in.Repo, Alerts: []fixloop.Alert{}}
	state, err := fixloop.CheckAlertsInput(in)
	if err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	out.State = state
	if err := kit.CheckString("project", in.Repo, maxRefLen, nil); err != nil {
		return nil, out, fmt.Errorf("check project: %w", err)
	}
	if !projectFormat.MatchString(in.Repo) || !fixloop.ValidPath(in.Repo) {
		return nil, out, fmt.Errorf("%w: project must be the path of the project, for example group/project", kit.ErrInvalidInput)
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("projects", in.Repo)}, &info); err != nil {
		return nil, out, fmt.Errorf("read the project (the project may not exist): %w", err)
	}
	if info.DefaultBranch == "" {
		return nil, out, errors.New("read the project: GitLab gave no default branch")
	}
	if in.Ref != "" && in.Ref != info.DefaultBranch {
		return nil, out, fmt.Errorf("%w: GitLab reports the vulnerabilities of the default branch only, which is %q, not %q",
			kit.ErrInvalidInput, info.DefaultBranch, in.Ref)
	}
	out.Ref = info.DefaultBranch

	managers, err := v.packageManagers(ctx, in.Repo)
	if err != nil {
		return nil, out, err
	}
	nodes, truncated, err := v.vulnerabilities(ctx, in.Repo, state)
	if err != nil {
		return nil, out, err
	}
	out.Truncated = truncated
	for _, n := range nodes {
		out.Alerts = append(out.Alerts, toAlert(n, managers))
	}
	return nil, out, nil
}

// packageManagers reads the dependency list. It returns the package manager of
// each name, version and file. It returns an error when the list is empty or when
// the token does not see the vulnerabilities.
func (v *service) packageManagers(ctx context.Context, project string) (map[string]string, error) {
	managers := map[string]string{}
	seen := 0
	page := "1"
	for range fixloop.MaxAlertPages {
		var list []glDependency
		var hdr http.Header
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: kit.Path("projects", project, "dependencies"),
			Query:          url.Values{"per_page": {strconv.Itoa(fixloop.AlertPageSize)}, "page": {page}},
			ResponseHeader: &hdr,
		}, &list)
		if err != nil {
			return nil, fmt.Errorf("list the dependencies (GitLab Ultimate and the read_api scope are required): %w", err)
		}
		for _, d := range list {
			seen++
			if d.Vulnerabilities == nil {
				return nil, errors.New("the dependency list has no vulnerabilities key: the token cannot read the Project Security Dashboard, " +
					"so the tool cannot tell a clean project from a project that it cannot see")
			}
			managers[depKey(d.Name, d.Version, d.FilePath)] = d.PackageManager
		}
		if page = hdr.Get("X-Next-Page"); page == "" || len(list) == 0 {
			break
		}
	}
	if seen == 0 {
		return nil, errors.New("the dependency list is empty: the project has no dependency scan, " +
			"so the tool cannot tell a clean project from a project that has no scan")
	}
	return managers, nil
}

func depKey(name, version, file string) string { return name + "\x00" + version + "\x00" + file }

const vulnerabilityQuery = `query($path: ID!, $state: [VulnerabilityState!], $after: String) {
  project(fullPath: $path) {
    vulnerabilities(reportType: [DEPENDENCY_SCANNING], state: $state, first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id state severity title webUrl
        identifiers { externalType externalId name url }
        location {
          __typename
          ... on VulnerabilityLocationDependencyScanning { file dependency { package { name } version } }
        }
      }
    }
  }
}`

type glVuln struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	WebURL      string `json:"webUrl"`
	Identifiers []struct {
		ExternalType string `json:"externalType"`
		ExternalID   string `json:"externalId"`
		Name         string `json:"name"`
		URL          string `json:"url"`
	} `json:"identifiers"`
	Location struct {
		File       string `json:"file"`
		Dependency *struct {
			Package *struct {
				Name string `json:"name"`
			} `json:"package"`
			Version string `json:"version"`
		} `json:"dependency"`
	} `json:"location"`
}

type glGraphResponse struct {
	Data *struct {
		Project *struct {
			Vulnerabilities *struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []glVuln `json:"nodes"`
			} `json:"vulnerabilities"`
		} `json:"project"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func graphStates(state string) []string {
	switch state {
	case fixloop.AlertOpen:
		return []string{"DETECTED", "CONFIRMED"}
	case fixloop.AlertFixed:
		return []string{"RESOLVED"}
	case fixloop.AlertDismissed:
		return []string{"DISMISSED"}
	}
	return nil
}

// vulnerabilities reads the pages of the GraphQL field. It returns true when
// GitLab has more pages than the tool reads.
func (v *service) vulnerabilities(ctx context.Context, project, state string) ([]glVuln, bool, error) {
	var all []glVuln
	after := ""
	for n := 0; ; n++ {
		if n == fixloop.MaxAlertPages {
			return all, true, nil
		}
		vars := map[string]any{"path": project, "state": graphStates(state)}
		if after != "" {
			vars["after"] = after
		}
		var res glGraphResponse
		err := v.gql.Do(ctx, kit.Request{
			Method: http.MethodPost, Path: "/graphql", Retry: true,
			JSON: map[string]any{"query": vulnerabilityQuery, "variables": vars},
		}, &res)
		if err != nil {
			return nil, false, fmt.Errorf("read the vulnerabilities (GitLab Ultimate and the read_api scope are required): %w", err)
		}
		if len(res.Errors) > 0 {
			msgs := make([]string, 0, maxGraphErrors)
			for _, e := range res.Errors[:min(len(res.Errors), maxGraphErrors)] {
				msgs = append(msgs, e.Message)
			}
			return nil, false, fmt.Errorf("read the vulnerabilities: GitLab answered GraphQL errors: %s", strings.Join(msgs, "; "))
		}
		if res.Data == nil || res.Data.Project == nil || res.Data.Project.Vulnerabilities == nil {
			return nil, false, errors.New("read the vulnerabilities: GitLab gave no vulnerabilities field. " +
				"The project does not exist, or the token cannot read its Security Dashboard")
		}
		vs := res.Data.Project.Vulnerabilities
		all = append(all, vs.Nodes...)
		if !vs.PageInfo.HasNextPage {
			return all, false, nil
		}
		if vs.PageInfo.EndCursor == "" {
			return nil, false, errors.New("read the vulnerabilities: GitLab has a next page but gave no cursor")
		}
		after = vs.PageInfo.EndCursor
	}
}

func toAlert(n glVuln, managers map[string]string) fixloop.Alert {
	al := fixloop.Alert{
		ID: "gitlab:" + n.ID[strings.LastIndex(n.ID, "/")+1:], State: alertState(n.State), Severity: strings.ToLower(n.Severity),
		ManifestPath: n.Location.File, URL: n.WebURL,
	}
	texts := make([]string, 0, 1+3*len(n.Identifiers))
	texts = append(texts, n.Title)
	for _, i := range n.Identifiers {
		texts = append(texts, i.Name, i.ExternalID, i.URL)
	}
	al.AdvisoryIDs = fixloop.AdvisoryIDs(texts...)
	al.Verifiable = len(al.AdvisoryIDs) > 0
	d := n.Location.Dependency
	if d == nil || d.Package == nil {
		return al
	}
	al.Package, al.Version = d.Package.Name, d.Version
	al.Ecosystem = managers[depKey(d.Package.Name, d.Version, n.Location.File)]
	if al.Ecosystem == "" {
		al.Ecosystem = fixloop.EcosystemOfFile(n.Location.File)
	}
	al.PurlType = fixloop.PurlType(al.Ecosystem)
	if al.Version != "" {
		al.VersionSource = fixloop.VersionFromHost
		al.Purl = fixloop.Purl(al.Ecosystem, al.Package, al.Version)
	}
	return al
}

// alertState maps VulnerabilityState to the state of an alert.
func alertState(s string) string {
	switch s {
	case "RESOLVED":
		return fixloop.AlertFixed
	case "DISMISSED":
		return fixloop.AlertDismissed
	}
	return fixloop.AlertOpen // DETECTED and CONFIRMED
}
