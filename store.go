package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Fixed scenario identity. The server serves one viewer and one org-free
// namespace: every repository belongs to this login.
// ---------------------------------------------------------------------------

const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

const fixedBaseMin = int64(1735689600) // 2025-01-01T00:00:00Z, unix seconds.

// stamp returns a deterministic ISO-8601 timestamp: base + n minutes.
func stamp(n int64) string {
	t := fixedBaseMin + n*60
	days := t / 86400
	rem := t % 86400
	y, m, d := civilFromDays(days)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, m, d, rem/3600, (rem%3600)/60, rem%60)
}

func civilFromDays(z int64) (y, m, d int64) {
	z += 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if mp >= 10 {
		m = mp - 9
		y++
	}
	return y, m, d
}

// ---------------------------------------------------------------------------
// JSON wire shapes (field order matches the reference wire format).
// ---------------------------------------------------------------------------

type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type repoJSON struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Private       bool     `json:"private"`
	Owner         userJSON `json:"owner"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	HTMLURL       string   `json:"html_url"`
	CloneURL      string   `json:"clone_url"`
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueJSON struct {
	ID        int64       `json:"id"`
	Number    int64       `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	HTMLURL   string      `json:"html_url"`
}

type commentJSON struct {
	ID        int64    `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string     `json:"created_at"`
}

type prHeadJSON struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type prJSON struct {
	ID             int64      `json:"id"`
	Number         int64      `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Head           prHeadJSON `json:"head"`
	Base           prHeadJSON `json:"base"`
	Merged         bool       `json:"merged"`
	MergedAt       string     `json:"merged_at,omitempty"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	CreatedAt      string     `json:"created_at"`
	HTMLURL        string     `json:"html_url"`
}

type contentJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
	Size     int64  `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

type branchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type refObjectJSON struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type refJSON struct {
	Ref    string        `json:"ref"`
	Object refObjectJSON `json:"object"`
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

const zeroOID = "0000000000000000000000000000000000000000"

func gitOID(typ byte, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", objTypeName(typ), len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func objTypeName(typ byte) string {
	switch typ {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "unknown"
}

func objTypeCode(name string) byte {
	switch name {
	case "commit":
		return objCommit
	case "tree":
		return objTree
	case "blob":
		return objBlob
	case "tag":
		return objTag
	}
	return 0
}

type gitObject struct {
	typ  byte
	data []byte
}

// ---------------------------------------------------------------------------
// Entities
// ---------------------------------------------------------------------------

type label struct {
	name  string
	color string
}

type issue struct {
	id        int64
	number    int64
	title     string
	body      string
	state     string // "open" | "closed"
	labels    []label
	createdAt string
	updatedAt string
	cached    []byte // pre-marshaled REST JSON; recomputed on mutation
}

type comment struct {
	id        int64
	body      string
	createdAt string
}

type pullRequest struct {
	id             int64
	number         int64
	title          string
	body           string
	state          string // "open" | "closed"
	merged         bool
	mergedAt       string
	mergeCommitSHA string
	headRef        string
	headSHA        string
	baseRef        string
	baseSHA        string
	createdAt      string
}

type repo struct {
	id          int64
	name        string
	description string
	private     bool
	createdAt   string
	updatedAt   string

	objects map[string]*gitObject // oid -> object
	refs    map[string]string     // "refs/heads/main" -> oid

	issues   []*issue
	issueByN map[int64]*issue
	comments map[int64][]*comment // issue number -> comments
	labels   map[string]*label
	prs      []*pullRequest
	prByN    map[int64]*pullRequest

	version uint64 // bumped on any ref/object change (cache invalidation)
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type store struct {
	mu sync.RWMutex

	repos   map[string]*repo // name -> repo (all owned by the viewer)
	repoSeq int64
	issSeq  int64
	cmtSeq  int64
	prSeq   int64
	clock   int64 // deterministic minute counter

	// fetch pack cache: key -> framed response body (sideband pack bytes).
	packCache sync.Map // string -> []byte
	packBuild sync.Map // string -> chan struct{} (singleflight)

	// advertisement cache: key -> response bytes
	advCache sync.Map // string -> []byte
}

var st = &store{repos: make(map[string]*repo)}

func newStore() *store {
	return &store{repos: make(map[string]*repo)}
}

func viewerUser() userJSON { return userJSON{Login: viewerLogin, ID: viewerID, Type: "User"} }

func (s *store) nextClock() int64 {
	v := atomic.AddInt64(&s.clock, 1)
	return v - 1
}

// lookupRepoLocked returns the repo for (owner,name); owner must be the viewer.
func (s *store) lookupRepoLocked(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[name]
}

func (s *store) createRepoLocked(name, description string, private bool) (*repo, error) {
	if _, ok := s.repos[name]; ok {
		return nil, errExists
	}
	s.repoSeq++
	n := s.nextClock()
	r := &repo{
		id:          s.repoSeq,
		name:        name,
		description: description,
		private:     private,
		createdAt:   stamp(n),
		updatedAt:   stamp(n),
		objects:     make(map[string]*gitObject),
		refs:        make(map[string]string),
		issueByN:    make(map[int64]*issue),
		comments:    make(map[int64][]*comment),
		labels:      make(map[string]*label),
		prByN:       make(map[int64]*pullRequest),
	}
	s.repos[name] = r
	return r, nil
}

func (s *store) deleteRepoLocked(name string) bool {
	if _, ok := s.repos[name]; !ok {
		return false
	}
	delete(s.repos, name)
	return true
}

func (r *repo) toJSON() repoJSON {
	return repoJSON{
		ID:            r.id,
		Name:          r.name,
		FullName:      viewerLogin + "/" + r.name,
		Private:       r.private,
		Owner:         viewerUser(),
		Description:   r.description,
		DefaultBranch: "main",
		CreatedAt:     r.createdAt,
		UpdatedAt:     r.updatedAt,
		HTMLURL:       "https://example.invalid/" + viewerLogin + "/" + r.name,
		CloneURL:      "https://example.invalid/" + viewerLogin + "/" + r.name + ".git",
	}
}

func (r *repo) defaultTip() (string, bool) {
	oid, ok := r.refs["refs/heads/main"]
	return oid, ok
}

// headRef returns the oid a ref name (short or full) resolves to.
func (r *repo) resolveRef(name string) (string, bool) {
	if oid, ok := r.refs[name]; ok {
		return oid, ok
	}
	if oid, ok := r.refs["refs/heads/"+name]; ok {
		return oid, ok
	}
	if oid, ok := r.refs["refs/tags/"+name]; ok {
		return oid, ok
	}
	return "", false
}

func (is *issue) toJSON(r *repo) issueJSON {
	labels := make([]labelJSON, len(is.labels))
	for i, l := range is.labels {
		labels[i] = labelJSON{Name: l.name, Color: l.color}
	}
	if labels == nil {
		labels = []labelJSON{}
	}
	return issueJSON{
		ID:        is.id,
		Number:    is.number,
		Title:     is.title,
		Body:      is.body,
		State:     is.state,
		Labels:    labels,
		User:      viewerUser(),
		CreatedAt: is.createdAt,
		UpdatedAt: is.updatedAt,
		HTMLURL:   "https://example.invalid/" + viewerLogin + "/" + r.name + "/issues/" + strconv.FormatInt(is.number, 10),
	}
}

// refreshCache rebuilds the pre-marshaled REST bytes for an issue.
func (is *issue) refreshCache(r *repo) {
	b, _ := json.Marshal(is.toJSON(r))
	is.cached = b
}

func (pr *pullRequest) toJSON(r *repo) prJSON {
	return prJSON{
		ID:     pr.id,
		Number: pr.number,
		Title:  pr.title,
		Body:   pr.body,
		State:  pr.state,
		Head:   prHeadJSON{Ref: pr.headRef, SHA: pr.headSHA},
		Base:   prHeadJSON{Ref: pr.baseRef, SHA: pr.baseSHA},
		Merged: pr.merged,
		MergedAt: func() string {
			if pr.merged {
				return pr.mergedAt
			}
			return ""
		}(),
		MergeCommitSHA: pr.mergeCommitSHA,
		CreatedAt:      pr.createdAt,
		HTMLURL:        "https://example.invalid/" + viewerLogin + "/" + r.name + "/pull/" + strconv.FormatInt(pr.number, 10),
	}
}

// globalID builds an opaque GraphQL node id: base64("<Type>:<payload>").
func globalID(parts ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
}

func repoGlobalID(r *repo) string    { return globalID("Repository", strconv.FormatInt(r.id, 10)) }
func issueGlobalID(r *repo, is *issue) string {
	return globalID("Issue", strconv.FormatInt(r.id, 10), strconv.FormatInt(is.number, 10))
}
func prGlobalID(r *repo, pr *pullRequest) string {
	return globalID("PullRequest", strconv.FormatInt(r.id, 10), strconv.FormatInt(pr.number, 10))
}
func commentGlobalID(r *repo, is *issue, c *comment) string {
	return globalID("IssueComment", strconv.FormatInt(r.id, 10), strconv.FormatInt(is.number, 10), strconv.FormatInt(c.id, 10))
}

// sortedRefNames returns ref names in deterministic (byte) order.
func (r *repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.refs))
	for n := range r.refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// parseCommitHeader extracts tree + parents from a commit object.
func parseCommitHeader(data []byte) (tree string, parents []string) {
	// headers are newline-terminated lines up to the first blank line
	i := 0
	for i < len(data) {
		j := bytes.IndexByte(data[i:], '\n')
		if j < 0 {
			break
		}
		line := string(data[i : i+j])
		i += j + 1
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			tree = line[5:]
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, line[7:])
		}
	}
	return tree, parents
}

// parseTreeEntries decodes a tree object: (mode, name, oid).
type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := strings.IndexByte(string(data[i:]), ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := strings.IndexByte(string(data[i:]), 0)
		if nul < 0 || i+nul+21 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
	}
	return out
}

// flattenTree returns path -> (mode, oid) for all blobs in a tree (recursive).
func (r *repo) flattenTree(treeOID string, prefix string, out map[string]treeEntry) {
	obj, ok := r.objects[treeOID]
	if !ok || obj.typ != objTree {
		return
	}
	for _, e := range parseTree(obj.data) {
		p := prefix + e.name
		if e.mode == "40000" {
			r.flattenTree(e.oid, p+"/", out)
		} else {
			out[p] = e
		}
	}
}

// commitTree returns the tree oid of a commit.
func (r *repo) commitTree(commitOID string) string {
	obj, ok := r.objects[commitOID]
	if !ok || obj.typ != objCommit {
		return ""
	}
	tree, _ := parseCommitHeader(obj.data)
	return tree
}

// ancestors returns the set of commit oids reachable from tip (including tip).
func (r *repo) ancestors(tip string) map[string]bool {
	seen := make(map[string]bool)
	var stack []string
	stack = append(stack, tip)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		_, parents := parseCommitHeader(obj.data)
		stack = append(stack, parents...)
	}
	return seen
}

// isAncestor reports whether anc is reachable from tip. Depth-first with
// early exit: for the common linear case (old = parent of new) this touches
// two commits instead of building the whole ancestor set.
func (r *repo) isAncestor(anc, tip string) bool {
	if anc == tip {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == anc {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		_, parents := parseCommitHeader(obj.data)
		stack = append(stack, parents...)
	}
	return false
}

// mergeBase finds the first common ancestor of a and b (BFS from a).
func (r *repo) mergeBase(a, b string) string {
	bAnc := r.ancestors(b)
	seen := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if bAnc[oid] {
			return oid
		}
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		_, parents := parseCommitHeader(obj.data)
		queue = append(queue, parents...)
	}
	return ""
}

// commitChain returns commits newest-first from tip following first parents.
func (r *repo) commitChain(tip string, max int) []string {
	var out []string
	seen := make(map[string]bool)
	cur := tip
	for cur != "" && len(out) < max {
		if seen[cur] {
			break
		}
		seen[cur] = true
		out = append(out, cur)
		obj, ok := r.objects[cur]
		if !ok || obj.typ != objCommit {
			break
		}
		_, parents := parseCommitHeader(obj.data)
		if len(parents) == 0 {
			break
		}
		cur = parents[0]
	}
	return out
}

// commitMessage returns the message part of a commit object (verbatim).
func commitMessage(data []byte) string {
	idx := strings.Index(string(data), "\n\n")
	if idx < 0 {
		return ""
	}
	return string(data[idx+2:])
}

var errExists = fmt.Errorf("already exists")
var errNotFound = fmt.Errorf("not found")
