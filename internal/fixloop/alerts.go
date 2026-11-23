package fixloop

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The states of a dependency alert.
const (
	AlertOpen      = "open"
	AlertFixed     = "fixed"
	AlertDismissed = "dismissed"
	// AlertAll is a filter value only. It selects each state.
	AlertAll = "all"
)

// Limits of list_dependency_alerts. A host gives 100 alerts on a page. The
// tools read at most MaxAlertPages pages, the same bound as the check runs of a
// commit. A longer list is cut and the result says so.
const (
	AlertPageSize = 100
	MaxAlertPages = 10
	MaxAlerts     = AlertPageSize * MaxAlertPages
	// MaxManifestReads bounds the manifest files that one call reads to find the
	// installed versions.
	MaxManifestReads = 20
)

// AlertsInput is the argument of list_dependency_alerts. Both Git hosts use it.
type AlertsInput struct {
	Repo  string `json:"project" jsonschema:"the repository: owner/repo on GitHub, group/project on GitLab"`
	State string `json:"state,omitempty" jsonschema:"open (the default), fixed, dismissed or all. GitLab lists open alerts only."`
	Ref   string `json:"ref,omitempty" jsonschema:"a branch. Both hosts report the default branch only, so a ref that is not the default branch is refused. Leave it out for the default branch."`
}

// Alert is one dependency alert of the Git host.
//
// Ecosystem is the name that the host uses: go, npm, pip, maven, rubygems,
// composer, nuget, rust, pub, swift, erlang, actions. PurlType is the same
// ecosystem as the type of a package URL (golang, npm, pypi, maven, gem and so
// on). Purl is the package URL with the installed version, for example
// pkg:npm/lodash@4.17.20. The trivy parser of the executor takes the package
// URL as the identity of a package, so Purl is the same string for the same
// package. Purl is empty when the version is not known.
//
// Version is the version that the ref installs. GitHub does not give it, so
// the GitHub tool reads it from the manifest of the alert at the default branch
// (go.mod, package-lock.json, requirements.txt) and VersionSource is
// "manifest". GitLab gives it in its dependency list and VersionSource is
// "host". When the tool cannot find it, Version is empty and VersionSource is
// empty.
type Alert struct {
	ID              string   `json:"id"`
	State           string   `json:"state"`
	Package         string   `json:"package"`
	Ecosystem       string   `json:"ecosystem"`
	PurlType        string   `json:"purl_type,omitempty"`
	Purl            string   `json:"purl,omitempty"`
	Version         string   `json:"version,omitempty"`
	VersionSource   string   `json:"version_source,omitempty"`
	VulnerableRange string   `json:"vulnerable_version_range,omitempty"`
	FirstPatched    string   `json:"first_patched_version,omitempty"`
	AdvisoryIDs     []string `json:"advisory_ids"`
	Severity        string   `json:"severity,omitempty"`
	ManifestPath    string   `json:"manifest_path,omitempty"`
	URL             string   `json:"url,omitempty"`
}

// AlertsResult is the result of list_dependency_alerts. Truncated is true when
// the host has more alerts than the tool read. The caller then does not know
// the whole list, and an alert that is missing is not a fixed alert.
type AlertsResult struct {
	Project   string  `json:"project"`
	Ref       string  `json:"ref"`
	State     string  `json:"state"`
	Alerts    []Alert `json:"alerts"`
	Truncated bool    `json:"truncated"`
	// Note explains a limit of the host, for example that a field is not known.
	Note string `json:"note,omitempty"`
}

// The values of Alert.VersionSource.
const (
	VersionFromManifest = "manifest"
	VersionFromHost     = "host"
)

// CheckAlertsInput checks the part of the input that does not depend on the
// host and returns the state to use.
func CheckAlertsInput(in AlertsInput) (state string, err error) {
	state = in.State
	if state == "" {
		state = AlertOpen
	}
	switch state {
	case AlertOpen, AlertFixed, AlertDismissed, AlertAll:
	default:
		return "", fmt.Errorf("%w: state must be open, fixed, dismissed or all", kit.ErrInvalidInput)
	}
	if in.Ref != "" {
		if err := CheckBranch("ref", in.Ref); err != nil {
			return "", err
		}
	}
	return state, nil
}

var (
	ghsaFormat = regexp.MustCompile(`(?i)\bGHSA(?:-[23456789cfghjmpqrvwx]{4}){3}\b`)
	cveFormat  = regexp.MustCompile(`(?i)\bCVE-\d{4}-\d{4,}\b`)
)

// AdvisoryIDs returns the GHSA ids and then the CVE ids that the texts hold,
// in upper case, with no repeat.
func AdvisoryIDs(texts ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, re := range []*regexp.Regexp{ghsaFormat, cveFormat} {
		for _, t := range texts {
			for _, id := range re.FindAllString(t, -1) {
				id = strings.ToUpper(id)
				if re == ghsaFormat {
					id = "GHSA" + strings.ToLower(id[4:])
				}
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// purlTypes maps the ecosystem names of GitHub and the package managers of
// GitLab to the type of a package URL.
var purlTypes = map[string]string{
	"go": "golang", "npm": "npm", "yarn": "npm", "pnpm": "npm", "pip": "pypi", "pipenv": "pypi", "setuptools": "pypi",
	"maven": "maven", "gradle": "maven", "sbt": "maven", "rubygems": "gem", "bundler": "gem", "rust": "cargo",
	"composer": "composer", "nuget": "nuget", "pub": "pub", "swift": "swift", "erlang": "hex", "actions": "githubactions",
	"conan": "conan",
}

// PurlType returns the package URL type of an ecosystem, or "" when it is not
// known.
func PurlType(ecosystem string) string { return purlTypes[strings.ToLower(ecosystem)] }

// Purl builds the package URL of a package at a version, in the form of the
// trivy parser: pkg:npm/lodash@4.17.20 and pkg:golang/github.com/x/y@v1.2.3.
// It returns "" when the ecosystem, the name or the version is empty or not
// known.
func Purl(ecosystem, name, version string) string {
	typ := PurlType(ecosystem)
	if typ == "" || name == "" || version == "" {
		return ""
	}
	if typ == "maven" {
		name = strings.Replace(name, ":", "/", 1)
	}
	if typ == "pypi" {
		name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	}
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(url.PathEscape(s), "@", "%40")
	}
	return "pkg:" + typ + "/" + strings.Join(segs, "/") + "@" + url.PathEscape(version)
}
