package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
	"github.com/zeroroot-ai/connectors/internal/kit"
)

// list_dependency_alerts uses the Dependabot alerts API:
//   GET /repos/{owner}/{repo}/dependabot/alerts?state=&per_page=100&after=
// https://docs.github.com/en/rest/dependabot/alerts#list-dependabot-alerts-for-a-repository
// The state filter is a comma list of auto_dismissed, dismissed, fixed and open.
// A page has at most 100 alerts. The page is by cursor: the Link header holds
// the next URL with an after parameter. Each alert has number, state,
// dependency{package{ecosystem,name},manifest_path,scope}, security_advisory{
// ghsa_id,cve_id,severity,identifiers[{type,value}]}, security_vulnerability{
// package,severity,vulnerable_version_range,first_patched_version{identifier}},
// url and html_url. The alert gives no installed version, so the tool reads it
// from the manifest of the alert at the default branch (see InstalledVersion).
// The alerts are the alerts of the default branch, so the tool refuses another
// ref. The token needs the Dependabot alerts read permission of the repository
// (fine-grained), or the security_events scope (classic).

func (v *service) registerAlerts(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_dependency_alerts",
		Description: "List the Dependabot alerts of a GitHub repository: the vulnerable dependencies that GitHub found in the default branch. " +
			"Read only. The state filter is open (the default), fixed, dismissed or all. Each alert has a stable id, the state, " +
			"the package, the ecosystem, the installed version and its package URL, the vulnerable version range, the first patched " +
			"version, the advisory ids (GHSA and CVE), the severity, the manifest path and the URL. " +
			"GitHub gives no installed version, so the tool reads it from go.mod, package-lock.json or requirements.txt at the default branch. " +
			"A package that the manifest installs at several versions has version_ambiguous true, the list in versions, and no purl. " +
			"The tool reads at most 1000 alerts. When there are more, truncated is true. " +
			"A ref that is not the default branch is refused. A GitHub error, such as disabled alerts (403), is an error and never an empty list. " +
			"To verify a fix, call it with state all and treat an alert as fixed only when its state is fixed. " +
			"An alert that is missing from the list, a dismissed alert and an auto dismissed alert (shown as dismissed) are not fixed. " +
			"A truncated list proves nothing.",
		Annotations: kit.ReadOnly("List GitHub dependency alerts"),
	}, v.listDependencyAlerts)
}

type ghAlert struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	HTMLURL    string `json:"html_url"`
	Dependency struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		ManifestPath string `json:"manifest_path"`
	} `json:"dependency"`
	Advisory struct {
		GHSAID      string `json:"ghsa_id"`
		CVEID       string `json:"cve_id"`
		Severity    string `json:"severity"`
		Identifiers []struct {
			Value string `json:"value"`
		} `json:"identifiers"`
	} `json:"security_advisory"`
	Vulnerability struct {
		Severity     string `json:"severity"`
		Range        string `json:"vulnerable_version_range"`
		FirstPatched *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
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
	owner, repo, ok := strings.Cut(in.Repo, "/")
	if !ok || !ownerFormat.MatchString(owner) || !validRepo(repo) {
		return nil, out, fmt.Errorf("%w: project must be owner/repo", kit.ErrInvalidInput)
	}
	r := prRef{owner: owner, repo: repo}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: repoPath(r)}, &info); err != nil {
		return nil, out, fmt.Errorf("read the repository (the project may not exist): %w", err)
	}
	if info.DefaultBranch == "" {
		return nil, out, errors.New("read the repository: GitHub gave no default branch")
	}
	if in.Ref != "" && in.Ref != info.DefaultBranch {
		return nil, out, fmt.Errorf("%w: GitHub reports dependency alerts for the default branch only, which is %q, not %q",
			kit.ErrInvalidInput, info.DefaultBranch, in.Ref)
	}
	out.Ref = info.DefaultBranch

	raw, truncated, err := v.fetchAlerts(ctx, r, state)
	if err != nil {
		return nil, out, err
	}
	out.Truncated = truncated
	out.Alerts = v.convertAlerts(ctx, in.Repo, info.DefaultBranch, raw)
	return nil, out, nil
}

// nextCursor returns the after value of the next link in a Link header. more is
// false when there is no next link. A next link that the tool cannot follow is an
// error, because ending the list there would show a cut list as a whole list.
func nextCursor(link string) (after string, more bool, err error) {
	for _, part := range strings.Split(link, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 || !strings.Contains(seg[1], `rel="next"`) {
			continue
		}
		u, perr := url.Parse(strings.Trim(strings.TrimSpace(seg[0]), "<>"))
		if perr != nil || u.Query().Get("after") == "" {
			return "", true, errors.New("list the dependency alerts: GitHub gave a next page link that has no after cursor, " +
				"so the tool cannot read the rest of the list")
		}
		return u.Query().Get("after"), true, nil
	}
	return "", false, nil
}

// convertAlerts makes the result alerts. It reads each manifest at most once.
func (v *service) convertAlerts(ctx context.Context, project, branch string, raw []ghAlert) []fixloop.Alert {
	out := make([]fixloop.Alert, 0, len(raw))
	manifests := map[string]string{}
	reads := 0
	for _, a := range raw {
		al := toAlert(a)
		if p := al.ManifestPath; p != "" {
			content, known := manifests[p]
			if !known && reads < fixloop.MaxManifestReads {
				reads++
				// A manifest that the tool cannot read leaves the version empty.
				if _, res, rerr := v.readFile(ctx, nil, fixloop.FileInput{Repo: project, Ref: branch, Path: p}); rerr == nil {
					content = res.Content
				}
				manifests[p] = content
			}
			applyInstalled(&al, fixloop.InstalledPackage(p, content, al.Package))
		}
		out = append(out, al)
	}
	return out
}

// applyInstalled sets the version of an alert. A package that the manifest
// installs at several versions has no single version. The alert then lists the
// versions and has no purl, so a caller does not match it to the wrong package.
func applyInstalled(al *fixloop.Alert, in fixloop.Installed) {
	switch len(in.Versions) {
	case 0:
	case 1:
		al.Version, al.VersionSource = in.Versions[0], fixloop.VersionFromManifest
		al.Purl = fixloop.Purl(al.Ecosystem, in.Name, in.Versions[0])
	default:
		al.Versions, al.VersionAmbiguous, al.VersionSource = in.Versions, true, fixloop.VersionFromManifest
	}
}

func toAlert(a ghAlert) fixloop.Alert {
	al := fixloop.Alert{
		ID: fmt.Sprintf("dependabot:%d", a.Number), State: a.State, Package: a.Dependency.Package.Name,
		Ecosystem: a.Dependency.Package.Ecosystem, VulnerableRange: a.Vulnerability.Range,
		Severity: a.Vulnerability.Severity, ManifestPath: a.Dependency.ManifestPath, URL: a.HTMLURL,
	}
	if al.State == "auto_dismissed" {
		al.State = fixloop.AlertDismissed
	}
	if al.Severity == "" {
		al.Severity = a.Advisory.Severity
	}
	if a.Vulnerability.FirstPatched != nil {
		al.FirstPatched = a.Vulnerability.FirstPatched.Identifier
	}
	ids := make([]string, 0, 2+len(a.Advisory.Identifiers))
	ids = append(ids, a.Advisory.GHSAID, a.Advisory.CVEID)
	for _, i := range a.Advisory.Identifiers {
		ids = append(ids, i.Value)
	}
	al.AdvisoryIDs = fixloop.AdvisoryIDs(ids...)
	al.Verifiable = len(al.AdvisoryIDs) > 0
	al.PurlType = fixloop.PurlType(al.Ecosystem)
	return al
}

// fetchAlerts reads the pages of alerts. It returns true when GitHub has more
// pages than the tool reads.
func (v *service) fetchAlerts(ctx context.Context, r prRef, state string) ([]ghAlert, bool, error) {
	q := url.Values{"per_page": {"100"}}
	switch state {
	case fixloop.AlertDismissed:
		q.Set("state", "dismissed,auto_dismissed")
	case fixloop.AlertOpen, fixloop.AlertFixed:
		q.Set("state", state)
	}
	var raw []ghAlert
	for page := 0; ; page++ {
		if page == fixloop.MaxAlertPages {
			return raw, true, nil
		}
		var list []ghAlert
		var hdr http.Header
		err := v.client.Do(ctx, kit.Request{
			Method: http.MethodGet, Path: repoPath(r, "dependabot", "alerts"), Query: q, ResponseHeader: &hdr,
		}, &list)
		if err != nil {
			return nil, false, fmt.Errorf("list the dependency alerts (the token needs the Dependabot alerts read permission): %w", err)
		}
		raw = append(raw, list...)
		after, more, err := nextCursor(hdr.Get("Link"))
		if err != nil {
			return nil, false, err
		}
		if !more {
			return raw, false, nil
		}
		q.Set("after", after)
	}
}
