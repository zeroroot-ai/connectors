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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// list_dependency_alerts uses the dependency list API:
//   GET /projects/:id/dependencies?per_page=100&page=N
// https://docs.gitlab.com/api/dependencies/
// The answer is an array of {"name","version","package_manager",
// "dependency_file_path","vulnerabilities":[{"name","severity","id","url"}]}.
// The list is the dependency list of the default branch. It needs GitLab
// Ultimate, the read_api scope, and the permission to read the Project Security
// Dashboard to see the vulnerabilities. The tool makes one alert of each
// vulnerability of a dependency.
//
// The vulnerability findings API (GET /projects/:id/vulnerability_findings,
// https://docs.gitlab.com/api/vulnerability_findings/) is deprecated, so the
// tool does not use it. The dependency list gives the installed version, so the
// tool needs no manifest read. It gives no vulnerable range, no patched version
// and no state: it lists the vulnerabilities that GitLab sees now, so each alert
// is open, and a vulnerability that a fix closed is no longer in the list. The
// tool takes the GHSA and CVE ids from the name and the URL of the vulnerability.
// The pages are the pages of the REST pagination: the X-Next-Page header is
// empty on the last page.

func (v *service) registerAlerts(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_dependency_alerts",
		Description: "List the vulnerable dependencies of a GitLab project: the vulnerabilities of its dependency list at the default branch. " +
			"Read only. It needs GitLab Ultimate. Each alert has a stable id, the state, the package, the ecosystem, the installed version " +
			"and its package URL, the advisory ids (GHSA and CVE) when the vulnerability names them, the severity, the manifest path and the URL. " +
			"GitLab gives no vulnerable range and no patched version, so those fields are empty. " +
			"The list holds the vulnerabilities that exist now, so each alert is open, and a fixed vulnerability is not in the list. " +
			"The state filter is open (the default) or all. The tool refuses fixed and dismissed. " +
			"The tool reads at most 1000 dependencies. When there are more, truncated is true. " +
			"A ref that is not the default branch is refused.",
		Annotations: kit.ReadOnly("List GitLab dependency alerts"),
	}, v.listDependencyAlerts)
}

type glDependency struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	PackageManager  string `json:"package_manager"`
	FilePath        string `json:"dependency_file_path"`
	Vulnerabilities []struct {
		Name     string      `json:"name"`
		Severity string      `json:"severity"`
		ID       json.Number `json:"id"`
		URL      string      `json:"url"`
	} `json:"vulnerabilities"`
}

func (v *service) listDependencyAlerts(ctx context.Context, _ *mcp.CallToolRequest, in fixloop.AlertsInput) (*mcp.CallToolResult, fixloop.AlertsResult, error) {
	out := fixloop.AlertsResult{Project: in.Repo, Alerts: []fixloop.Alert{}}
	state, err := fixloop.CheckAlertsInput(in)
	if err != nil {
		return nil, out, fmt.Errorf("check input: %w", err)
	}
	out.State = state
	if state == fixloop.AlertFixed || state == fixloop.AlertDismissed {
		return nil, out, fmt.Errorf("%w: the dependency list of GitLab holds open vulnerabilities only. "+
			"Use state open, and read a fix as an alert that is gone", kit.ErrInvalidInput)
	}
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
		return nil, out, fmt.Errorf("%w: GitLab reports the dependency list of the default branch only, which is %q, not %q",
			kit.ErrInvalidInput, info.DefaultBranch, in.Ref)
	}
	out.Ref = info.DefaultBranch

	page := "1"
	for n := 0; ; n++ {
		if n == fixloop.MaxAlertPages {
			out.Truncated = true
			break
		}
		var list []glDependency
		var hdr http.Header
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: kit.Path("projects", in.Repo, "dependencies"),
			Query:          url.Values{"per_page": {strconv.Itoa(fixloop.AlertPageSize)}, "page": {page}},
			ResponseHeader: &hdr,
		}, &list)
		if err != nil {
			return nil, out, fmt.Errorf("list the dependencies (GitLab Ultimate and the read_api scope are required): %w", err)
		}
		for _, d := range list {
			for _, vu := range d.Vulnerabilities {
				al := fixloop.Alert{
					ID: "gitlab:" + vu.ID.String(), State: fixloop.AlertOpen, Package: d.Name, Ecosystem: d.PackageManager,
					PurlType: fixloop.PurlType(d.PackageManager), Version: d.Version, ManifestPath: d.FilePath,
					Severity: vu.Severity, URL: vu.URL, AdvisoryIDs: fixloop.AdvisoryIDs(vu.Name, vu.URL),
				}
				if d.Version != "" {
					al.VersionSource = fixloop.VersionFromHost
					al.Purl = fixloop.Purl(d.PackageManager, d.Name, d.Version)
				}
				out.Alerts = append(out.Alerts, al)
			}
		}
		page = hdr.Get("X-Next-Page")
		if page == "" || len(list) == 0 {
			break
		}
	}
	return nil, out, nil
}
