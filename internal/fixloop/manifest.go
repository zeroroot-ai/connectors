package fixloop

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// InstalledVersion returns the version of a package that a manifest or a lock
// file installs, or "" when the file does not name it. GitHub gives no
// installed version in a Dependabot alert, so the GitHub tool reads it here.
// It knows three files:
//   - go.mod: the version of the require line of the module.
//   - package-lock.json: the version of node_modules/<name> (lockfile version 2
//     and 3), or of the dependencies entry (version 1).
//   - requirements.txt: the version of a line name==version.
//
// A range is not an installed version, so a file that holds only a range gives
// "".
func InstalledVersion(manifestPath, content, name string) string {
	switch path.Base(manifestPath) {
	case "go.mod":
		return goModVersion(content, name)
	case "package-lock.json":
		return npmLockVersion(content, name)
	case "requirements.txt":
		return requirementsVersion(content, name)
	}
	return ""
}

func goModVersion(content, name string) string {
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case !inBlock && strings.HasPrefix(line, "require "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		case !inBlock:
			continue
		}
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == name {
			return f[1]
		}
	}
	return ""
}

func npmLockVersion(content, name string) string {
	var lock struct {
		Packages     map[string]struct{ Version string } `json:"packages"`
		Dependencies map[string]struct{ Version string } `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(content), &lock); err != nil {
		return ""
	}
	if p, ok := lock.Packages["node_modules/"+name]; ok {
		return p.Version
	}
	return lock.Dependencies[name].Version
}

var requirementLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^\]]*\])?\s*==\s*([^\s;#]+)`)

func normalizePip(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(s))
}

func requirementsVersion(content, name string) string {
	for _, line := range strings.Split(content, "\n") {
		if m := requirementLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil && normalizePip(m[1]) == normalizePip(name) {
			return m[2]
		}
	}
	return ""
}
