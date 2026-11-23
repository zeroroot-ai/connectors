package mockforge

import (
	"net/http"
	"strconv"
	"strings"
)

// Dependabot alerts. The endpoint follows
// https://docs.github.com/en/rest/dependabot/alerts#list-dependabot-alerts-for-a-repository
//
// The mock derives the alerts from the content of the repository. A test seeds
// advisories with POST /_mock/advisories. The mock reads go.mod on the default
// branch. An alert is open while go.mod requires a version of the advisory
// package that the vulnerable range holds. It is fixed after a commit moves
// the default branch to a version that the range does not hold, or removes the
// requirement. A merged pull request is such a commit, so a fix closes the
// alert with no other action. The mock supports the ecosystem go only.

// Advisory is a security advisory that a test seeds.
type Advisory struct {
	GHSAID          string `json:"ghsa_id"`
	CVEID           string `json:"cve_id"`
	Package         string `json:"package"`
	Ecosystem       string `json:"ecosystem"`
	VulnerableRange string `json:"vulnerable_version_range"`
	FirstPatched    string `json:"first_patched_version"`
	Severity        string `json:"severity"`
	Summary         string `json:"summary"`
}

// alert is the state of one advisory in one repository.
type alert struct {
	Number      int
	Advisory    Advisory
	State       string // open, fixed or dismissed
	CreatedAt   string
	FixedAt     string
	DismissedAt string
}

// evalAlerts brings the alerts of a repository in line with go.mod on the
// default branch.
func (m *Mock) evalAlerts(r *repo) {
	sha, ok := r.refs[r.defaultBranch]
	if !ok {
		return
	}
	var gomod string
	if e, ok := r.treeOf(sha)["go.mod"]; ok {
		gomod = r.blobs[e.SHA]
	}
	at := stamp(m.now())
	for _, adv := range m.advisories {
		ver := goModRequire(gomod, adv.Package)
		vulnerable := ver != "" && inRange(ver, adv.VulnerableRange)
		a := r.alerts[adv.GHSAID]
		switch {
		case vulnerable && a == nil:
			r.nextAlert++
			r.alerts[adv.GHSAID] = &alert{Number: r.nextAlert, Advisory: adv, State: "open", CreatedAt: at}
		case vulnerable && a.State == "fixed":
			a.State, a.FixedAt = "open", ""
		case !vulnerable && a != nil && a.State == "open":
			a.State, a.FixedAt = "fixed", at
		}
	}
}

func (r *repo) alertList() []*alert {
	list := make([]*alert, 0, len(r.alerts))
	for _, a := range r.alerts {
		list = append(list, a)
	}
	for i := 1; i < len(list); i++ { // insertion sort by number, newest first
		for j := i; j > 0 && list[j].Number > list[j-1].Number; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list
}

func (c *call) alertJSON(a *alert) map[string]any {
	base := c.scheme() + "://" + c.host
	adv := a.Advisory
	ids := []map[string]string{{"type": "GHSA", "value": adv.GHSAID}}
	if adv.CVEID != "" {
		ids = append(ids, map[string]string{"type": "CVE", "value": adv.CVEID})
	}
	var patched any
	if adv.FirstPatched != "" {
		patched = map[string]string{"identifier": adv.FirstPatched}
	}
	pkg := map[string]string{"ecosystem": adv.Ecosystem, "name": adv.Package}
	var cve any
	if adv.CVEID != "" {
		cve = adv.CVEID
	}
	var fixed, dismissed any
	if a.FixedAt != "" {
		fixed = a.FixedAt
	}
	if a.DismissedAt != "" {
		dismissed = a.DismissedAt
	}
	n := strconv.Itoa(a.Number)
	return map[string]any{
		"number": a.Number, "state": a.State,
		"dependency": map[string]any{"package": pkg, "manifest_path": "go.mod", "scope": "runtime"},
		"security_advisory": map[string]any{
			"ghsa_id": adv.GHSAID, "cve_id": cve, "summary": adv.Summary, "severity": adv.Severity, "identifiers": ids,
		},
		"security_vulnerability": map[string]any{
			"package": pkg, "severity": adv.Severity, "vulnerable_version_range": adv.VulnerableRange,
			"first_patched_version": patched,
		},
		"url":        base + "/repos/" + c.r.fullName() + "/dependabot/alerts/" + n,
		"html_url":   base + "/" + c.r.fullName() + "/security/dependabot/" + n,
		"created_at": a.CreatedAt, "fixed_at": fixed, "dismissed_at": dismissed,
	}
}

// alerts serves GET /repos/{o}/{r}/dependabot/alerts. The paging is by cursor,
// as on GitHub: the Link header holds the after cursor of the next page.
func (c *call) alerts(rest []string) {
	if len(rest) != 1 || rest[0] != "alerts" || c.req.Method != http.MethodGet {
		notFound(c.w)
		return
	}
	q := c.req.URL.Query()
	want := map[string]bool{}
	for _, s := range strings.Split(q.Get("state"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	list := []*alert{}
	for _, a := range c.r.alertList() {
		if len(want) == 0 || want[a.State] {
			list = append(list, a)
		}
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per < 1 || per > 100 {
		per = 30
	}
	from, _ := strconv.Atoi(q.Get("after"))
	from = max(0, min(from, len(list)))
	to := min(from+per, len(list))
	if to < len(list) {
		next := c.scheme() + "://" + c.host + c.req.URL.Path + "?per_page=" + strconv.Itoa(per) + "&after=" + strconv.Itoa(to)
		if s := q.Get("state"); s != "" {
			next += "&state=" + s
		}
		c.w.Header().Set("Link", "<"+next+`>; rel="next"`)
	}
	out := make([]map[string]any, 0, to-from)
	for _, a := range list[from:to] {
		out = append(out, c.alertJSON(a))
	}
	writeJSON(c.w, http.StatusOK, out)
}

// ---- control ----

// addAdvisory serves POST /_mock/advisories.
func (m *Mock) addAdvisory(w http.ResponseWriter, req *http.Request) {
	var in Advisory
	if !readBody(w, req, &in) {
		return
	}
	if in.GHSAID == "" || in.Package == "" || in.VulnerableRange == "" {
		apiError(w, http.StatusBadRequest, "ghsa_id, package and vulnerable_version_range are required")
		return
	}
	if in.Ecosystem == "" {
		in.Ecosystem = "go"
	}
	if in.Ecosystem != "go" {
		apiError(w, http.StatusBadRequest, "the mock reads go.mod only: the ecosystem is go")
		return
	}
	if in.Severity == "" {
		in.Severity = "high"
	}
	for _, a := range m.advisories {
		if a.GHSAID == in.GHSAID {
			apiError(w, http.StatusConflict, "the advisory exists already")
			return
		}
	}
	m.advisories = append(m.advisories, in)
	for _, r := range m.repos {
		m.evalAlerts(r)
	}
	writeJSON(w, http.StatusCreated, in)
}

// dismissAlert serves POST /_mock/repos/{o}/{r}/alerts/{number}/dismiss.
func (m *Mock) dismissAlert(w http.ResponseWriter, r *repo, number string) {
	n, _ := strconv.Atoi(number)
	for _, a := range r.alerts {
		if a.Number == n && a.State == "open" {
			a.State, a.DismissedAt = "dismissed", stamp(m.now())
			writeJSON(w, http.StatusOK, map[string]any{"number": n, "state": a.State})
			return
		}
	}
	notFound(w)
}

// ---- go.mod and versions ----

// goModRequire returns the version that go.mod requires for a module.
func goModRequire(content, module string) string {
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
			line = strings.TrimPrefix(line, "require ")
		case !inBlock:
			continue
		}
		if f := strings.Fields(line); len(f) == 2 && f[0] == module {
			return f[1]
		}
	}
	return ""
}

// inRange reports whether a version is in a vulnerable range of GitHub. The
// range is comparisons that a comma joins, for example ">= 1.0.0, < 1.2.4".
func inRange(version, rng string) bool {
	for _, part := range strings.Split(rng, ",") {
		part = strings.TrimSpace(part)
		op := ""
		for _, o := range []string{">=", "<=", "<", ">", "="} {
			if strings.HasPrefix(part, o) {
				op = o
				break
			}
		}
		if op == "" {
			return false
		}
		c := compareVersions(version, strings.TrimSpace(strings.TrimPrefix(part, op)))
		ok := map[string]bool{">=": c >= 0, "<=": c <= 0, "<": c < 0, ">": c > 0, "=": c == 0}[op]
		if !ok {
			return false
		}
	}
	return rng != ""
}

// compareVersions compares the numeric parts of two versions. It ignores a v
// prefix and a suffix after - or +.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, s := range parts {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}
