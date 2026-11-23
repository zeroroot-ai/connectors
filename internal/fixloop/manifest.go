package fixloop

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Installed is what a manifest installs for a package. Name is the name that
// gives the identity of the package. It differs from the asked name when a go.mod
// replace directive swaps the module. Versions is empty when the file does not
// name an installed version. It has more than one entry when the package is
// installed at several versions, so the caller must not pick one.
type Installed struct {
	Name     string
	Versions []string
}

// InstalledPackage returns the versions of a package that a manifest or a lock
// file installs. GitHub gives no installed version in a Dependabot alert, so the
// GitHub tool and the mock read it here. It knows three files:
//   - go.mod: the require lines, with the replace directives applied as the trivy
//     parser does. A replace to a module gives the name and the version of the
//     new module. A replace to a local path gives no version.
//   - package-lock.json: each node_modules/<name> entry, nested copies too
//     (lockfile version 2 and 3), and each dependencies entry, nested ones too
//     (version 1).
//   - requirements.txt: a line name==version or name===version. A range and a
//     wildcard such as 1.2.* are not an installed version.
func InstalledPackage(manifestPath, content, name string) Installed {
	switch path.Base(manifestPath) {
	case "go.mod":
		return goModInstalled(content, name)
	case "package-lock.json":
		return Installed{Name: name, Versions: npmLockVersions(content, name)}
	case "requirements.txt":
		in := Installed{Name: name}
		if v := requirementsVersion(content, name); v != "" {
			in.Versions = []string{v}
		}
		return in
	}
	return Installed{Name: name}
}

type goReplace struct{ old, oldVer, new, newVer string }

func goModLines(content string) (requires [][2]string, replaces []goReplace) {
	block := ""
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "require (" || line == "replace (":
			block = strings.TrimSuffix(line, " (")
			continue
		case block != "" && line == ")":
			block = ""
			continue
		}
		kind, rest := block, line
		if kind == "" {
			k, r, ok := strings.Cut(line, " ")
			if !ok || (k != "require" && k != "replace") {
				continue
			}
			kind, rest = k, strings.TrimSpace(r)
		}
		if kind == "require" {
			if f := strings.Fields(rest); len(f) == 2 {
				requires = append(requires, [2]string{f[0], f[1]})
			}
			continue
		}
		left, right, ok := strings.Cut(rest, "=>")
		l, r := strings.Fields(left), strings.Fields(right)
		if !ok || len(l) == 0 || len(l) > 2 || len(r) == 0 || len(r) > 2 {
			continue
		}
		rep := goReplace{old: l[0], new: r[0]}
		if len(l) == 2 {
			rep.oldVer = l[1]
		}
		if len(r) == 2 {
			rep.newVer = r[1]
		}
		replaces = append(replaces, rep)
	}
	return requires, replaces
}

func goModInstalled(content, name string) Installed {
	requires, replaces := goModLines(content)
	out := Installed{Name: name}
	for _, rq := range requires {
		eName, eVer := rq[0], rq[1]
		for _, rp := range replaces {
			if rp.old == rq[0] && (rp.oldVer == "" || rp.oldVer == rq[1]) {
				eName, eVer = rp.new, rp.newVer // a local path has no version
				break
			}
		}
		if rq[0] != name && eName != name {
			continue
		}
		if eVer == "" {
			out.Name = rq[0]
			continue
		}
		out.Name = eName
		if !slices.Contains(out.Versions, eVer) {
			out.Versions = append(out.Versions, eVer)
		}
	}
	return out
}

type npmDep struct {
	Version      string            `json:"version"`
	Dependencies map[string]npmDep `json:"dependencies"`
}

func npmLockVersions(content, name string) []string {
	var lock struct {
		Packages     map[string]npmDep `json:"packages"`
		Dependencies map[string]npmDep `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(content), &lock); err != nil {
		return nil
	}
	var out []string
	add := func(v string) {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	for key, p := range lock.Packages {
		if key == "node_modules/"+name || strings.HasSuffix(key, "/node_modules/"+name) {
			add(p.Version)
		}
	}
	var walk func(deps map[string]npmDep)
	walk = func(deps map[string]npmDep) {
		for n, d := range deps {
			if n == name {
				add(d.Version)
			}
			walk(d.Dependencies)
		}
	}
	walk(lock.Dependencies)
	slices.Sort(out)
	return out
}

var requirementLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^\]]*\])?\s*===?\s*([^\s;#,]+)\s*(?:[;#].*)?$`)

func normalizePip(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(s))
}

func requirementsVersion(content, name string) string {
	for _, line := range strings.Split(content, "\n") {
		m := requirementLine.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) != 3 || normalizePip(m[1]) != normalizePip(name) {
			continue
		}
		if strings.ContainsAny(m[2], "*=<>!~") {
			return "" // a wildcard or a range, no installed version
		}
		return m[2]
	}
	return ""
}
