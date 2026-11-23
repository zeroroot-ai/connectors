package mockforge

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedNumbers(m map[int]*pull) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// head returns the head commit. An open pull request follows its branch. A
// closed one keeps the last head.
func (p *pull) head(r *repo) string {
	if p.State == "open" {
		if sha, ok := r.refs[p.HeadRef]; ok {
			p.HeadSHA = sha
		}
	}
	return p.HeadSHA
}

func (p *pull) base(r *repo) string {
	if p.State == "open" {
		if sha, ok := r.refs[p.BaseRef]; ok {
			p.BaseSHA = sha
		}
	}
	return p.BaseSHA
}

// commitList returns the commits of the pull request, oldest first. A merged
// pull request keeps the list that it had at the merge.
func (p *pull) commitList(r *repo) []string {
	if p.frozenList != nil {
		return p.frozenList
	}
	return r.commitsBetween(p.base(r), p.head(r))
}

// mergeable reports whether the head merges into the base with no conflict.
func (p *pull) mergeable(r *repo) bool {
	_, ok := r.mergedTree(p)
	return ok
}

func (r *repo) mergedTree(p *pull) (map[string]entry, bool) {
	base, head := p.base(r), p.head(r)
	return threeWay(r.treeOf(r.mergeBase(base, head)), r.treeOf(base), r.treeOf(head))
}

func (c *call) pullJSON(p *pull) map[string]any {
	r := c.r
	head, base := p.head(r), p.base(r)
	var mergeable any
	state := "unknown"
	if p.State == "open" {
		if p.mergeable(r) {
			mergeable, state = true, "clean"
		} else {
			mergeable, state = false, "dirty"
		}
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	repoJSON := map[string]string{"full_name": r.fullName()}
	return map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": p.State, "merged": p.Merged,
		"html_url":   fmt.Sprintf("%s://%s/%s/pull/%d", c.scheme(), c.host, r.fullName(), p.Number),
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "closed_at": nullable(p.ClosedAt),
		"merged_at": nullable(p.MergedAt), "merge_commit_sha": nullable(p.MergeSHA),
		"mergeable": mergeable, "mergeable_state": state, "commits": len(p.commitList(r)),
		"head": map[string]any{"sha": head, "ref": p.HeadRef, "repo": repoJSON},
		"base": map[string]any{"sha": base, "ref": p.BaseRef, "repo": repoJSON},
	}
}

func (c *call) pulls(rest []string) {
	switch {
	case len(rest) == 0 && c.req.Method == http.MethodGet:
		c.listPulls()
	case len(rest) == 0 && c.req.Method == http.MethodPost:
		c.createPull()
	case len(rest) >= 1:
		n, err := strconv.Atoi(rest[0])
		p := c.r.pulls[n]
		if err != nil || p == nil {
			notFound(c.w)
			return
		}
		c.pullOf(p, rest[1:])
	default:
		notFound(c.w)
	}
}

func (c *call) pullOf(p *pull, rest []string) {
	switch {
	case len(rest) == 0 && c.req.Method == http.MethodGet:
		writeJSON(c.w, http.StatusOK, c.pullJSON(p))
	case len(rest) == 0 && c.req.Method == http.MethodPatch:
		c.patchPull(p)
	case len(rest) == 1 && rest[0] == "merge" && c.req.Method == http.MethodPut:
		c.mergePull(p)
	case len(rest) == 1 && rest[0] == "commits" && c.req.Method == http.MethodGet:
		c.pullCommits(p)
	default:
		notFound(c.w)
	}
}

func (c *call) listPulls() {
	q := c.req.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	_, headBranch, _ := strings.Cut(q.Get("head"), ":")
	out := []map[string]any{}
	for i := len(c.r.pulls); i >= 1; i-- { // newest first
		p := c.r.pulls[i]
		switch {
		case p == nil, state != "all" && p.State != state:
		case q.Get("head") != "" && p.HeadRef != headBranch:
		case q.Get("base") != "" && p.BaseRef != q.Get("base"):
		default:
			out = append(out, c.pullJSON(p))
		}
	}
	from, to := page(c.req, len(out))
	writeJSON(c.w, http.StatusOK, out[from:to])
}

func (c *call) createPull() {
	var in struct{ Title, Body, Head, Base string }
	if !readBody(c.w, c.req, &in) {
		return
	}
	head := in.Head
	if _, b, ok := strings.Cut(in.Head, ":"); ok {
		head = b
	}
	if _, ok := c.r.refs[head]; !ok {
		apiError(c.w, http.StatusUnprocessableEntity, "Validation Failed: head invalid")
		return
	}
	if _, ok := c.r.refs[in.Base]; !ok {
		apiError(c.w, http.StatusUnprocessableEntity, "Validation Failed: base invalid")
		return
	}
	for _, p := range c.r.pulls {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == in.Base {
			apiError(c.w, http.StatusUnprocessableEntity, "Validation Failed: A pull request already exists for "+c.r.owner+":"+head+".")
			return
		}
	}
	if c.r.refs[head] == c.r.refs[in.Base] {
		apiError(c.w, http.StatusUnprocessableEntity, "Validation Failed: No commits between "+in.Base+" and "+head)
		return
	}
	now := stamp(c.m.now())
	p := &pull{Number: c.r.nextNumber, Title: in.Title, Body: in.Body, State: "open", HeadRef: head, BaseRef: in.Base,
		HeadSHA: c.r.refs[head], BaseSHA: c.r.refs[in.Base], CreatedAt: now, UpdatedAt: now}
	c.r.nextNumber++
	c.r.pulls[p.Number] = p
	writeJSON(c.w, http.StatusCreated, c.pullJSON(p))
}

func (c *call) patchPull(p *pull) {
	var in struct{ State, Title string }
	if !readBody(c.w, c.req, &in) {
		return
	}
	now := stamp(c.m.now())
	if in.Title != "" {
		p.Title = in.Title
	}
	switch {
	case in.State == "closed" && p.State == "open":
		p.head(c.r)
		p.base(c.r)
		p.State, p.ClosedAt = "closed", now
	case in.State == "open" && p.State == "closed" && !p.Merged:
		p.State, p.ClosedAt = "open", ""
	}
	p.UpdatedAt = now
	writeJSON(c.w, http.StatusOK, c.pullJSON(p))
}

func (c *call) pullCommits(p *pull) {
	all := p.commitList(c.r)
	from, to := page(c.req, len(all))
	out := []map[string]any{}
	for _, s := range all[from:to] {
		cm := c.r.commits[s]
		out = append(out, map[string]any{"sha": s, "commit": map[string]any{"message": cm.Message, "author": cm.Author}})
	}
	writeJSON(c.w, http.StatusOK, out)
}

// mergePull follows "merge a pull request". The answer 409 says that the sha is
// not the head, and 405 says that the pull request is not mergeable.
func (c *call) mergePull(p *pull) {
	var in struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	if !readBody(c.w, c.req, &in) {
		return
	}
	r := c.r
	if f := r.mergeFail; f != nil {
		apiError(c.w, f.Status, f.Message)
		return
	}
	switch {
	case p.State != "open":
		apiError(c.w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	case in.SHA != "" && in.SHA != p.head(r):
		apiError(c.w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
		return
	}
	method := in.MergeMethod
	if method == "" {
		method = "merge"
	}
	if msg := r.methodRefusal(method); msg != "" {
		apiError(c.w, http.StatusMethodNotAllowed, msg)
		return
	}
	tree, ok := r.mergedTree(p)
	if !ok {
		apiError(c.w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	baseTip, head := p.base(r), p.head(r)
	list := p.commitList(r)
	now := c.m.now()
	author := person{Name: mockName, Email: mockEmail, Date: stamp(now)}
	var cm *commit
	if method == "squash" {
		cm = r.putCommit(fmt.Sprintf("%s (#%d)", p.Title, p.Number), r.putTree(tree), []string{baseTip}, author)
	} else {
		msg := fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s", p.Number, r.owner, p.HeadRef, p.Title)
		cm = r.putCommit(msg, r.putTree(tree), []string{baseTip, head}, author)
	}
	r.refs[p.BaseRef] = cm.SHA
	p.frozenList = append([]string{}, list...)
	p.State, p.Merged, p.MergeSHA = "closed", true, cm.SHA
	p.MergedAt, p.ClosedAt, p.UpdatedAt = stamp(now), stamp(now), stamp(now)
	r.afterLand(p.BaseRef, cm.SHA, stamp(now))
	writeJSON(c.w, http.StatusOK, map[string]any{"sha": cm.SHA, "merged": true, "message": "Pull Request successfully merged"})
}

func (r *repo) methodRefusal(method string) string {
	switch method {
	case "merge":
		if !r.allowMerge {
			return "Merge commits are not allowed on this repository."
		}
	case "squash":
		if !r.allowSquash {
			return "Squash merges are not allowed on this repository."
		}
	default:
		return "The mock does not support the merge method " + method + "."
	}
	return ""
}

// afterLand makes the deployment of a commit that lands on the default branch,
// when the repository has an auto deploy.
func (r *repo) afterLand(branch, sha, at string) {
	if r.autoDeploy != nil && branch == r.defaultBranch {
		r.addDeployment(sha, r.autoDeploy.Environment, r.autoDeploy.State, at)
	}
}
