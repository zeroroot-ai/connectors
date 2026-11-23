package mockforge

import (
	"crypto/sha1" //nolint:gosec // git names an object with SHA-1, this is no security use
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zeroroot-ai/connectors/internal/fixloop"
)

// The git model of one repository. It holds blobs, flat trees (a path maps to
// one blob entry), commits and branches. A tree holds only blobs, so a path
// with a slash is one entry and the mock has no folder object.

const (
	// FileMode is the mode of a regular file.
	FileMode = "100644"
	// MockEmail is the author of a commit that the mock makes on its own.
	mockName  = "mock-forge"
	mockEmail = "mock-forge@example.invalid"
)

type entry struct {
	Mode string `json:"mode"`
	SHA  string `json:"sha"`
}

type person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commit struct {
	SHA     string
	Message string
	Author  person
	Tree    string
	Parents []string
	seq     int
}

type repo struct {
	owner, name   string
	defaultBranch string
	allowMerge    bool
	allowSquash   bool
	allowRebase   bool
	required      []string
	defaultChecks []Check
	checks        map[string][]Check
	autoDeploy    *AutoDeploy
	mergeFail     *Failure

	blobs   map[string]string
	trees   map[string]map[string]entry
	commits map[string]*commit
	refs    map[string]string

	pulls       map[int]*pull
	nextNumber  int
	deployments []*deployment
	nextDeploy  int
	seq         int

	alerts    map[string]*alert // by GHSA id
	nextAlert int
	alertsOff bool
}

func newRepo(owner, name, branch string) *repo {
	return &repo{
		owner: owner, name: name, defaultBranch: branch,
		allowMerge: true, allowSquash: true,
		defaultChecks: []Check{{Name: "ci", Status: "completed", Conclusion: "success"}},
		checks:        map[string][]Check{},
		alerts:        map[string]*alert{},
		blobs:         map[string]string{}, trees: map[string]map[string]entry{},
		commits: map[string]*commit{}, refs: map[string]string{},
		pulls: map[int]*pull{}, nextNumber: 1, nextDeploy: 1,
	}
}

func (r *repo) fullName() string { return r.owner + "/" + r.name }

func sha1hex(parts ...string) string {
	h := sha1.New() //nolint:gosec // see the import
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *repo) putBlob(content string) string {
	sha := fixloop.BlobSHA(content)
	r.blobs[sha] = content
	return sha
}

func (r *repo) putTree(entries map[string]entry) string {
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		e := entries[p]
		fmt.Fprintf(&b, "%s %s\x00%s", e.Mode, p, e.SHA)
	}
	sha := sha1hex("tree ", b.String())
	cp := make(map[string]entry, len(entries))
	for p, e := range entries {
		cp[p] = e
	}
	r.trees[sha] = cp
	return sha
}

func (r *repo) putCommit(message, tree string, parents []string, author person) *commit {
	sha := sha1hex("commit ", tree, "\n", strings.Join(parents, ","), "\n", author.Name, author.Email, author.Date, "\n", message)
	if c, ok := r.commits[sha]; ok {
		return c
	}
	r.seq++
	c := &commit{SHA: sha, Message: message, Author: author, Tree: tree, Parents: append([]string{}, parents...), seq: r.seq}
	r.commits[sha] = c
	return c
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ancestors returns the commit and every commit that it reaches, in the order
// of a breadth-first walk.
func (r *repo) ancestors(sha string) []string {
	var order []string
	seen := map[string]bool{}
	queue := []string{sha}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		c, ok := r.commits[cur]
		if !ok || seen[cur] {
			continue
		}
		seen[cur] = true
		order = append(order, cur)
		queue = append(queue, c.Parents...)
	}
	return order
}

func toSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

// mergeBase returns the nearest commit that both commits reach, or "".
func (r *repo) mergeBase(a, b string) string {
	inA := toSet(r.ancestors(a))
	for _, s := range r.ancestors(b) {
		if inA[s] {
			return s
		}
	}
	return ""
}

// commitsBetween returns the commits that head reaches and base does not,
// oldest first.
func (r *repo) commitsBetween(base, head string) []string {
	skip := toSet(r.ancestors(base))
	var out []string
	for _, s := range r.ancestors(head) {
		if !skip[s] {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return r.commits[out[i]].seq < r.commits[out[j]].seq })
	return out
}

func (r *repo) treeOf(sha string) map[string]entry {
	if c, ok := r.commits[sha]; ok {
		return r.trees[c.Tree]
	}
	return nil
}

// resolve turns a branch name or a commit sha into a commit sha.
func (r *repo) resolve(name string) (string, bool) {
	if sha, ok := r.refs[name]; ok {
		return sha, true
	}
	if _, ok := r.commits[name]; ok {
		return name, true
	}
	return "", false
}

// fileChange is one file of a compare answer.
type fileChange struct {
	Path, Status, SHA, Mode, Previous string
}

// diff lists the files that differ between two trees, sorted by path. The
// status "changed" is a change of the mode with the same content, as GitHub
// reports it. The mock does not detect a rename.
func diff(base, head map[string]entry) []fileChange {
	var out []fileChange
	for p, h := range head {
		b, ok := base[p]
		switch {
		case !ok:
			out = append(out, fileChange{Path: p, Status: "added", SHA: h.SHA, Mode: h.Mode})
		case b.SHA != h.SHA:
			out = append(out, fileChange{Path: p, Status: "modified", SHA: h.SHA, Mode: h.Mode})
		case b.Mode != h.Mode:
			out = append(out, fileChange{Path: p, Status: "changed", SHA: h.SHA, Mode: h.Mode})
		}
	}
	for p, b := range base {
		if _, ok := head[p]; !ok {
			out = append(out, fileChange{Path: p, Status: "removed", SHA: b.SHA, Mode: b.Mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// threeWay applies the change from mb to head onto base. It reports false when
// a path changed on both sides to different entries.
func threeWay(mb, base, head map[string]entry) (map[string]entry, bool) {
	out := make(map[string]entry, len(base))
	for p, e := range base {
		out[p] = e
	}
	paths := map[string]bool{}
	for p := range mb {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	for p := range paths {
		m, inMB := mb[p]
		h, inHead := head[p]
		if inMB == inHead && m == h {
			continue // head did not change this path
		}
		b, inBase := base[p]
		switch {
		case inBase == inMB && b == m: // base did not change it: take the head
			if inHead {
				out[p] = h
			} else {
				delete(out, p)
			}
		case inBase == inHead && b == h: // both sides made the same change
		default:
			return nil, false
		}
	}
	return out, true
}
