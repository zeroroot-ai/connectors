package mockforge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The control API. A test seeds the state and reads it here. All paths start
// with /_mock/, which no real API uses.
//
//	GET    /_mock/state                               the repositories and the health values
//	POST   /_mock/reset                               drop all state
//	POST   /_mock/repos                               seed a repository
//	GET    /_mock/repos/{owner}/{repo}                read the repository: files of each branch, pulls, deployments
//	PATCH  /_mock/repos/{owner}/{repo}                change the settings
//	POST   /_mock/repos/{owner}/{repo}/commits        push a commit to a branch
//	POST   /_mock/repos/{owner}/{repo}/checks         set the check runs of a commit, or the default
//	PUT    /_mock/repos/{owner}/{repo}/merge-failure  make each merge call fail
//	DELETE /_mock/repos/{owner}/{repo}/merge-failure  clear it
//	POST   /_mock/repos/{owner}/{repo}/deployments    record a deployment
//	POST   /_mock/prometheus/health                   set a health value
//	POST   /_mock/prometheus/alerts                   set the alerts

// FileSpec is the content of a file in a control call. In JSON it is a string
// (a regular file) or an object {"content": "...", "mode": "100755"}. A null
// value in a map of files deletes the path.
type FileSpec struct {
	Content string `json:"content"`
	Mode    string `json:"mode,omitempty"`
}

// UnmarshalJSON reads a string or an object.
func (f *FileSpec) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		f.Content, f.Mode = s, ""
		return nil
	}
	type plain FileSpec
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("a file is a string or an object with content and mode: %w", err)
	}
	*f = FileSpec(p)
	return nil
}

// Settings are the switches of one repository. A field that is absent keeps
// its value.
type Settings struct {
	AllowMerge     *bool       `json:"allow_merge_commit"`
	AllowSquash    *bool       `json:"allow_squash_merge"`
	AllowRebase    *bool       `json:"allow_rebase_merge"`
	RequiredChecks *[]string   `json:"required_checks"`
	Checks         *[]Check    `json:"checks"`
	AutoDeploy     *AutoDeploy `json:"auto_deploy"`
}

func (r *repo) apply(s Settings) {
	if s.AllowMerge != nil {
		r.allowMerge = *s.AllowMerge
	}
	if s.AllowSquash != nil {
		r.allowSquash = *s.AllowSquash
	}
	if s.AllowRebase != nil {
		r.allowRebase = *s.AllowRebase
	}
	if s.RequiredChecks != nil {
		r.required = *s.RequiredChecks
	}
	if s.Checks != nil {
		r.defaultChecks = *s.Checks
	}
	if s.AutoDeploy != nil {
		r.autoDeploy = s.AutoDeploy
		if s.AutoDeploy.Environment == "" {
			r.autoDeploy = nil
		}
	}
}

func (r *repo) writeFiles(tree map[string]entry, files map[string]*FileSpec) {
	for p, f := range files {
		if f == nil {
			delete(tree, p)
			continue
		}
		mode := f.Mode
		if mode == "" {
			mode = FileMode
		}
		tree[p] = entry{Mode: mode, SHA: r.putBlob(f.Content)}
	}
}

func (m *Mock) control(w http.ResponseWriter, req *http.Request, segs []string) {
	switch {
	case len(segs) == 1 && segs[0] == "state" && req.Method == http.MethodGet:
		m.state(w)
	case len(segs) == 1 && segs[0] == "reset" && req.Method == http.MethodPost:
		m.reset()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case len(segs) == 1 && segs[0] == "repos" && req.Method == http.MethodPost:
		m.seed(w, req)
	case len(segs) >= 3 && segs[0] == "repos":
		r := m.repo(segs[1], segs[2])
		if r == nil {
			notFound(w)
			return
		}
		m.controlRepo(w, req, r, segs[3:])
	case len(segs) == 2 && segs[0] == "prometheus":
		m.controlProm(w, req, segs[1])
	default:
		notFound(w)
	}
}

func (m *Mock) seed(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Owner         string               `json:"owner"`
		Repo          string               `json:"repo"`
		DefaultBranch string               `json:"default_branch"`
		Files         map[string]*FileSpec `json:"files"`
		Settings
	}
	if !readBody(w, req, &in) {
		return
	}
	if in.Owner == "" || in.Repo == "" || strings.ContainsAny(in.Owner+in.Repo, "/ ") {
		apiError(w, http.StatusBadRequest, "owner and repo are required and hold no slash")
		return
	}
	if in.DefaultBranch == "" {
		in.DefaultBranch = "main"
	}
	if m.repo(in.Owner, in.Repo) != nil {
		apiError(w, http.StatusConflict, "the repository exists already")
		return
	}
	r := newRepo(in.Owner, in.Repo, in.DefaultBranch)
	r.apply(in.Settings)
	tree := map[string]entry{}
	r.writeFiles(tree, in.Files)
	author := person{Name: mockName, Email: mockEmail, Date: stamp(m.now())}
	c := r.putCommit("Initial commit", r.putTree(tree), nil, author)
	r.refs[in.DefaultBranch] = c.SHA
	m.repos[strings.ToLower(r.fullName())] = r
	writeJSON(w, http.StatusCreated, map[string]any{"full_name": r.fullName(), "branch": in.DefaultBranch, "head_sha": c.SHA, "files": blobShas(tree)})
}

func blobShas(tree map[string]entry) map[string]string {
	out := map[string]string{}
	for p, e := range tree {
		out[p] = e.SHA
	}
	return out
}

func (m *Mock) controlRepo(w http.ResponseWriter, req *http.Request, r *repo, rest []string) {
	switch {
	case len(rest) == 0 && req.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, m.repoState(r))
	case len(rest) == 0 && req.Method == http.MethodPatch:
		var s Settings
		if readBody(w, req, &s) {
			r.apply(s)
			writeJSON(w, http.StatusOK, m.repoState(r))
		}
	case len(rest) == 1 && rest[0] == "commits" && req.Method == http.MethodPost:
		m.push(w, req, r)
	case len(rest) == 1 && rest[0] == "checks" && req.Method == http.MethodPost:
		m.setChecks(w, req, r)
	case len(rest) == 1 && rest[0] == "merge-failure" && req.Method == http.MethodPut:
		var f Failure
		if readBody(w, req, &f) {
			if f.Status < 400 || f.Status > 599 {
				apiError(w, http.StatusBadRequest, "status must be an HTTP error status")
				return
			}
			r.mergeFail = &f
			writeJSON(w, http.StatusOK, f)
		}
	case len(rest) == 1 && rest[0] == "merge-failure" && req.Method == http.MethodDelete:
		r.mergeFail = nil
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case len(rest) == 1 && rest[0] == "deployments" && req.Method == http.MethodPost:
		m.deploy(w, req, r)
	default:
		notFound(w)
	}
}

// push makes a commit on a branch. A branch that does not exist starts at the
// default branch.
func (m *Mock) push(w http.ResponseWriter, req *http.Request, r *repo) {
	var in struct {
		Branch  string               `json:"branch"`
		Message string               `json:"message"`
		Files   map[string]*FileSpec `json:"files"`
	}
	if !readBody(w, req, &in) {
		return
	}
	if in.Branch == "" {
		in.Branch = r.defaultBranch
	}
	parent, ok := r.refs[in.Branch]
	if !ok {
		parent = r.refs[r.defaultBranch]
	}
	tree := map[string]entry{}
	for p, e := range r.treeOf(parent) {
		tree[p] = e
	}
	r.writeFiles(tree, in.Files)
	if in.Message == "" {
		in.Message = "Push from the test"
	}
	author := person{Name: mockName, Email: mockEmail, Date: stamp(m.now())}
	c := r.putCommit(in.Message, r.putTree(tree), []string{parent}, author)
	r.refs[in.Branch] = c.SHA
	r.afterLand(in.Branch, c.SHA, stamp(m.now()))
	writeJSON(w, http.StatusCreated, map[string]string{"branch": in.Branch, "sha": c.SHA})
}

func (r *repo) target(sha, branch string) (string, bool) {
	switch {
	case sha != "":
		_, ok := r.commits[sha]
		return sha, ok
	case branch != "":
		s, ok := r.refs[branch]
		return s, ok
	}
	return "", true
}

// setChecks sets the check runs of one commit, or the default of the repository
// when the call names no commit and no branch.
func (m *Mock) setChecks(w http.ResponseWriter, req *http.Request, r *repo) {
	var in struct {
		SHA    string  `json:"sha"`
		Branch string  `json:"branch"`
		Checks []Check `json:"checks"`
	}
	if !readBody(w, req, &in) {
		return
	}
	sha, ok := r.target(in.SHA, in.Branch)
	if !ok {
		notFound(w)
		return
	}
	if in.Checks == nil {
		in.Checks = []Check{}
	}
	if sha == "" {
		r.defaultChecks = in.Checks
	} else {
		r.checks[sha] = in.Checks
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "checks": in.Checks})
}

func (m *Mock) deploy(w http.ResponseWriter, req *http.Request, r *repo) {
	var in struct {
		SHA         string `json:"sha"`
		Branch      string `json:"branch"`
		Environment string `json:"environment"`
		State       string `json:"state"`
	}
	if !readBody(w, req, &in) {
		return
	}
	sha, ok := r.target(in.SHA, in.Branch)
	if !ok || sha == "" || in.Environment == "" {
		apiError(w, http.StatusBadRequest, "name a commit or a branch that exists, and an environment")
		return
	}
	if in.State == "" {
		in.State = "success"
	}
	d := r.addDeployment(sha, in.Environment, in.State, stamp(m.now()))
	writeJSON(w, http.StatusCreated, map[string]any{"id": d.ID, "sha": sha})
}

func (m *Mock) repoState(r *repo) map[string]any {
	branches := map[string]any{}
	for _, name := range sortedKeys(r.refs) {
		files := map[string]any{}
		for p, e := range r.treeOf(r.refs[name]) {
			files[p] = map[string]string{"content": r.blobs[e.SHA], "mode": e.Mode, "sha": e.SHA}
		}
		branches[name] = map[string]any{"sha": r.refs[name], "files": files}
	}
	pulls := []map[string]any{}
	for _, n := range sortedNumbers(r.pulls) {
		p := r.pulls[n]
		pulls = append(pulls, map[string]any{
			"number": n, "title": p.Title, "state": p.State, "merged": p.Merged, "head_ref": p.HeadRef,
			"head_sha": p.head(r), "base_ref": p.BaseRef, "merge_commit": p.MergeSHA,
		})
	}
	deps := []map[string]any{}
	for _, d := range r.deployments {
		deps = append(deps, map[string]any{"id": d.ID, "sha": d.SHA, "environment": d.Environment, "statuses": d.Statuses})
	}
	return map[string]any{
		"full_name": r.fullName(), "default_branch": r.defaultBranch, "branches": branches, "pulls": pulls,
		"deployments": deps, "merge_failure": r.mergeFail, "auto_deploy": r.autoDeploy,
		"allow_merge_commit": r.allowMerge, "allow_squash_merge": r.allowSquash, "required_checks": r.required,
		"default_checks": r.defaultChecks,
	}
}

func (m *Mock) state(w http.ResponseWriter) {
	repos := []string{}
	for _, r := range m.repos {
		repos = append(repos, r.fullName())
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "prometheus": m.promView()})
}
