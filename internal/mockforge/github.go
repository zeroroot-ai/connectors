package mockforge

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

// The endpoints follow the GitHub REST API, version 2022-11-28. Only the calls
// of the github-fixloop server exist. The pages:
//   - pulls: https://docs.github.com/en/rest/pulls/pulls
//   - check runs: https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference
//   - combined status: https://docs.github.com/en/rest/commits/statuses#get-the-combined-status-for-a-specific-reference
//   - git data: https://docs.github.com/en/rest/git/commits, .../git/refs, .../git/blobs, .../git/trees
//   - deployments: https://docs.github.com/en/rest/deployments/deployments and .../deployments/statuses
//   - compare: https://docs.github.com/en/rest/commits/commits#compare-two-commits
//   - pull requests of a commit: https://docs.github.com/en/rest/commits/commits#list-pull-requests-associated-with-a-commit
//   - repository, branch and branch rules: https://docs.github.com/en/rest/repos/repos#get-a-repository,
//     https://docs.github.com/en/rest/branches/branches#get-a-branch and
//     https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch
//   - contents: https://docs.github.com/en/rest/repos/contents#get-repository-content

// call is one request to a repository.
type call struct {
	m    *Mock
	r    *repo
	w    http.ResponseWriter
	req  *http.Request
	host string
}

func (m *Mock) github(w http.ResponseWriter, req *http.Request, segs []string) {
	if len(segs) < 3 || segs[0] != "repos" {
		notFound(w)
		return
	}
	r := m.repo(segs[1], segs[2])
	if r == nil {
		notFound(w)
		return
	}
	c := &call{m: m, r: r, w: w, req: req, host: req.Host}
	rest := segs[3:]
	if len(rest) == 0 {
		c.getRepo()
		return
	}
	switch rest[0] {
	case "pulls":
		c.pulls(rest[1:])
	case "commits":
		c.commitsOf(rest[1:])
	case "branches":
		c.branch(strings.Join(rest[1:], "/"))
	case "rules":
		writeJSON(w, http.StatusOK, []any{}) // no ruleset
	case "git":
		c.git(rest[1:])
	case "compare":
		c.compare(strings.Join(rest[1:], "/"))
	case "deployments":
		c.deployments(rest[1:])
	case "contents":
		c.contents(strings.Join(rest[1:], "/"))
	default:
		notFound(w)
	}
}

func (c *call) scheme() string {
	if c.req.TLS != nil {
		return "https"
	}
	return "http"
}

func (c *call) getRepo() {
	writeJSON(c.w, http.StatusOK, map[string]any{
		"full_name": c.r.fullName(), "default_branch": c.r.defaultBranch,
		"allow_merge_commit": c.r.allowMerge, "allow_squash_merge": c.r.allowSquash,
		"allow_rebase_merge": c.r.allowRebase,
	})
}

func (c *call) branch(name string) {
	if _, ok := c.r.refs[name]; !ok || name == "" {
		notFound(c.w)
		return
	}
	req := c.r.required
	if req == nil {
		req = []string{}
	}
	writeJSON(c.w, http.StatusOK, map[string]any{
		"name": name, "protection": map[string]any{"required_status_checks": map[string]any{"contexts": req}},
	})
}

// ---- git data ----

func (c *call) git(rest []string) {
	switch {
	case len(rest) >= 3 && rest[0] == "ref" && rest[1] == "heads" && c.req.Method == http.MethodGet:
		name := strings.Join(rest[2:], "/")
		sha, ok := c.r.refs[name]
		if !ok {
			notFound(c.w)
			return
		}
		writeJSON(c.w, http.StatusOK, map[string]any{"ref": "refs/heads/" + name, "object": map[string]string{"type": "commit", "sha": sha}})
	default:
		c.gitObjects(rest)
	}
}

func (c *call) gitObjects(rest []string) {
	post := c.req.Method == http.MethodPost
	switch {
	case len(rest) == 1 && rest[0] == "refs" && post:
		c.createRef()
	case len(rest) == 1 && rest[0] == "commits" && post:
		c.createCommit()
	case len(rest) == 2 && rest[0] == "commits" && c.req.Method == http.MethodGet:
		c.getCommit(rest[1])
	case len(rest) == 1 && rest[0] == "blobs" && post:
		c.createBlob()
	case len(rest) == 1 && rest[0] == "trees" && post:
		c.createTree()
	case len(rest) == 2 && rest[0] == "trees" && c.req.Method == http.MethodGet:
		c.getTree(rest[1])
	default:
		notFound(c.w)
	}
}

func (c *call) createRef() {
	var in struct{ Ref, SHA string }
	if !readBody(c.w, c.req, &in) {
		return
	}
	name, ok := strings.CutPrefix(in.Ref, "refs/heads/")
	if !ok || name == "" {
		apiError(c.w, http.StatusUnprocessableEntity, "Validation Failed: ref must start with refs/heads/")
		return
	}
	if _, exists := c.r.refs[name]; exists {
		apiError(c.w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	if _, ok := c.r.commits[in.SHA]; !ok {
		apiError(c.w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	c.r.refs[name] = in.SHA
	writeJSON(c.w, http.StatusCreated, map[string]any{"ref": in.Ref, "object": map[string]string{"type": "commit", "sha": in.SHA}})
}

func commitJSON(c *commit) map[string]any {
	parents := make([]map[string]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, map[string]string{"sha": p})
	}
	return map[string]any{
		"sha": c.SHA, "message": c.Message, "author": c.Author, "committer": c.Author,
		"tree": map[string]string{"sha": c.Tree}, "parents": parents,
	}
}

func (c *call) getCommit(sha string) {
	cm, ok := c.r.commits[sha]
	if !ok {
		notFound(c.w)
		return
	}
	writeJSON(c.w, http.StatusOK, commitJSON(cm))
}

func (c *call) createCommit() {
	var in struct {
		Message string
		Tree    string
		Parents []string
		Author  *person
	}
	if !readBody(c.w, c.req, &in) {
		return
	}
	if _, ok := c.r.trees[in.Tree]; !ok {
		apiError(c.w, http.StatusUnprocessableEntity, "Tree SHA does not exist")
		return
	}
	for _, p := range in.Parents {
		if _, ok := c.r.commits[p]; !ok {
			apiError(c.w, http.StatusUnprocessableEntity, "Parent does not exist")
			return
		}
	}
	author := person{Name: mockName, Email: mockEmail, Date: stamp(c.m.now())}
	if in.Author != nil {
		author = *in.Author
	}
	writeJSON(c.w, http.StatusCreated, commitJSON(c.r.putCommit(in.Message, in.Tree, in.Parents, author)))
}

func (c *call) createBlob() {
	var in struct{ Content, Encoding string }
	if !readBody(c.w, c.req, &in) {
		return
	}
	content := in.Content
	if in.Encoding == "base64" {
		raw, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			apiError(c.w, http.StatusUnprocessableEntity, "Content is not valid base64")
			return
		}
		content = string(raw)
	}
	writeJSON(c.w, http.StatusCreated, map[string]string{"sha": c.r.putBlob(content)})
}

func (c *call) createTree() {
	var in struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path string
			Mode string
			Type string
			SHA  *string
		}
	}
	if !readBody(c.w, c.req, &in) {
		return
	}
	entries := map[string]entry{}
	if in.BaseTree != "" {
		base, ok := c.r.trees[in.BaseTree]
		if !ok {
			apiError(c.w, http.StatusUnprocessableEntity, "base_tree does not exist")
			return
		}
		for p, e := range base {
			entries[p] = e
		}
	}
	for _, t := range in.Tree {
		if t.SHA == nil {
			delete(entries, t.Path)
			continue
		}
		if _, ok := c.r.blobs[*t.SHA]; !ok {
			apiError(c.w, http.StatusUnprocessableEntity, "Tree entry has a blob that does not exist")
			return
		}
		entries[t.Path] = entry{Mode: t.Mode, SHA: *t.SHA}
	}
	writeJSON(c.w, http.StatusCreated, map[string]string{"sha": c.r.putTree(entries)})
}

func (c *call) getTree(sha string) {
	tree, ok := c.r.trees[sha]
	if !ok {
		notFound(c.w)
		return
	}
	list := make([]map[string]string, 0, len(tree))
	for _, p := range sortedKeys(tree) {
		list = append(list, map[string]string{"path": p, "mode": tree[p].Mode, "type": "blob", "sha": tree[p].SHA})
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"sha": sha, "truncated": false, "tree": list})
}

// ---- compare ----

func (c *call) compare(spec string) {
	baseName, headName, ok := strings.Cut(spec, "...")
	base, okBase := c.r.resolve(baseName)
	head, okHead := c.r.resolve(headName)
	if !ok || !okBase || !okHead {
		notFound(c.w)
		return
	}
	ahead := c.r.commitsBetween(base, head)
	behind := c.r.commitsBetween(head, base)
	status := "diverged"
	switch {
	case base == head:
		status = "identical"
	case len(behind) == 0:
		status = "ahead"
	case len(ahead) == 0:
		status = "behind"
	}
	mb := c.r.mergeBase(base, head)
	files := []map[string]string{}
	for _, f := range diff(c.r.treeOf(mb), c.r.treeOf(head)) {
		files = append(files, map[string]string{"filename": f.Path, "status": f.Status, "sha": f.SHA})
	}
	commits := make([]map[string]string, 0, len(ahead))
	for _, s := range ahead {
		commits = append(commits, map[string]string{"sha": s})
	}
	writeJSON(c.w, http.StatusOK, map[string]any{
		"status": status, "ahead_by": len(ahead), "behind_by": len(behind), "total_commits": len(ahead),
		"commits": commits, "files": files,
	})
}

// ---- contents ----

func (c *call) contents(path string) {
	ref := c.req.URL.Query().Get("ref")
	if ref == "" {
		ref = c.r.defaultBranch
	}
	sha, ok := c.r.resolve(ref)
	if !ok {
		notFound(c.w)
		return
	}
	tree := c.r.treeOf(sha)
	if e, ok := tree[path]; ok {
		content := c.r.blobs[e.SHA]
		writeJSON(c.w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "size": len(content), "sha": e.SHA, "path": path,
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		})
		return
	}
	var list []map[string]any
	for _, p := range sortedKeys(tree) {
		if strings.HasPrefix(p, strings.TrimSuffix(path, "/")+"/") {
			list = append(list, map[string]any{"type": "file", "path": p, "sha": tree[p].SHA})
		}
	}
	if len(list) == 0 {
		notFound(c.w)
		return
	}
	writeJSON(c.w, http.StatusOK, list)
}

// ---- checks and statuses ----

func (c *call) commitsOf(rest []string) {
	if len(rest) != 2 || c.req.Method != http.MethodGet {
		notFound(c.w)
		return
	}
	sha := rest[0]
	if _, ok := c.r.commits[sha]; !ok {
		apiError(c.w, http.StatusUnprocessableEntity, "No commit found for SHA: "+sha)
		return
	}
	switch rest[1] {
	case "check-runs":
		list := c.r.checksOf(sha)
		from, to := page(c.req, len(list))
		writeJSON(c.w, http.StatusOK, map[string]any{"total_count": len(list), "check_runs": list[from:to]})
	case "status":
		writeJSON(c.w, http.StatusOK, map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
	case "pulls":
		c.pullsOfCommit(sha)
	default:
		notFound(c.w)
	}
}

func (r *repo) checksOf(sha string) []Check {
	if list, ok := r.checks[sha]; ok {
		return list
	}
	return r.defaultChecks
}

func (c *call) pullsOfCommit(sha string) {
	list := []map[string]int{}
	for _, n := range sortedNumbers(c.r.pulls) {
		for _, s := range c.r.pulls[n].commitList(c.r) {
			if s == sha {
				list = append(list, map[string]int{"number": n})
				break
			}
		}
	}
	writeJSON(c.w, http.StatusOK, list)
}

// ---- deployments ----

func (c *call) deployments(rest []string) {
	q := c.req.URL.Query()
	if len(rest) == 0 {
		list := []map[string]any{}
		for i := len(c.r.deployments) - 1; i >= 0; i-- { // newest first
			d := c.r.deployments[i]
			if (q.Get("environment") != "" && q.Get("environment") != d.Environment) || (q.Get("sha") != "" && q.Get("sha") != d.SHA) {
				continue
			}
			list = append(list, map[string]any{"id": d.ID, "sha": d.SHA, "environment": d.Environment, "created_at": d.CreatedAt})
		}
		writeJSON(c.w, http.StatusOK, list)
		return
	}
	id, err := strconv.Atoi(rest[0])
	if err != nil || len(rest) != 2 || rest[1] != "statuses" {
		notFound(c.w)
		return
	}
	for _, d := range c.r.deployments {
		if d.ID == id {
			writeJSON(c.w, http.StatusOK, d.Statuses)
			return
		}
	}
	notFound(c.w)
}

func (r *repo) addDeployment(sha, env, state, at string) *deployment {
	d := &deployment{ID: r.nextDeploy, SHA: sha, Environment: env, CreatedAt: at,
		Statuses: []depStatus{{State: state, CreatedAt: at}}}
	r.nextDeploy++
	r.deployments = append(r.deployments, d)
	return d
}
