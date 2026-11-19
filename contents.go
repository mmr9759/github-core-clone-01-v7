package main

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Commit / tree construction (contents API + merges)
// ---------------------------------------------------------------------------

const forgeAuthor = "forge <forge@example.invalid> 1735689600 +0000"

// buildCommitLocked creates a commit object with the given parent ("" = none),
// tree and message (stored verbatim, no trailing newline added), and returns
// its oid. Caller holds the store write lock.
func (r *repo) buildCommitLocked(parent, treeOID, message string) string {
	var sb strings.Builder
	sb.WriteString("tree " + treeOID + "\n")
	if parent != "" {
		sb.WriteString("parent " + parent + "\n")
	}
	sb.WriteString("author " + forgeAuthor + "\n")
	sb.WriteString("committer " + forgeAuthor + "\n")
	sb.WriteString("\n")
	sb.WriteString(message)
	data := []byte(sb.String())
	oid := gitOID(objCommit, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: objCommit, data: data}
	}
	return oid
}

// buildTreeLocked builds (or reuses) a tree object from path -> blob oid.
// mode is always 100644 for files created through the API.
func (r *repo) buildTreeLocked(files map[string]string) string {
	// group by directory
	dirs := map[string]map[string]string{}
	var rootFiles []string
	for path := range files {
		if i := strings.IndexByte(path, '/'); i >= 0 {
			d := path[:i]
			rest := path[i+1:]
			if _, ok := dirs[d]; !ok {
				dirs[d] = map[string]string{}
			}
			dirs[d][rest] = files[path]
		} else {
			rootFiles = append(rootFiles, path)
		}
	}
	type ent struct {
		mode string
		name string
		oid  string
	}
	var entries []ent
	for _, f := range rootFiles {
		entries = append(entries, ent{"100644", f, files[f]})
	}
	dirNames := make([]string, 0, len(dirs))
	for d := range dirs {
		dirNames = append(dirNames, d)
	}
	sort.Strings(dirNames)
	for _, d := range dirNames {
		sub := r.buildTreeLocked(dirs[d])
		entries = append(entries, ent{"40000", d, sub})
	}
	// git tree order: names sorted with directories as name+"/"
	sort.Slice(entries, func(i, j int) bool {
		ni, nj := entries[i].name, entries[j].name
		if entries[i].mode == "40000" {
			ni += "/"
		}
		if entries[j].mode == "40000" {
			nj += "/"
		}
		return ni < nj
	})
	var buf strings.Builder
	for _, e := range entries {
		buf.WriteString(e.mode + " " + e.name + "\x00")
		raw, _ := hex.DecodeString(e.oid)
		buf.Write(raw)
	}
	data := []byte(buf.String())
	oid := gitOID(objTree, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: objTree, data: data}
	}
	return oid
}

// treeToFiles flattens the tree at treeOID into path -> blob oid.
func (r *repo) treeToFiles(treeOID string) map[string]string {
	flat := map[string]treeEntry{}
	r.flattenTree(treeOID, "", flat)
	out := map[string]string{}
	for p, e := range flat {
		out[p] = e.oid
	}
	return out
}

// ---------------------------------------------------------------------------
// Contents API
// ---------------------------------------------------------------------------

func handleContents(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	path := strings.Join(tail, "/")
	// URL-decode the path segments
	if u, err := decodePath(path); err == nil {
		path = u
	}
	switch r.Method {
	case "GET":
		handleGetContents(w, r, owner, name, path)
	case "PUT":
		handlePutContents(w, r, owner, name, path)
	case "DELETE":
		handleDeleteContents(w, r, owner, name, path)
	default:
		writeJSON(w, 404, errJSON("Not Found"))
	}
}

func decodePath(p string) (string, error) {
	// segments are already URL-decoded by net/http except %2F; keep it simple
	return strings.ReplaceAll(p, "%2F", "/"), nil
}

func (r *repo) contentsTip(ref string) (string, bool) {
	if ref == "" {
		return r.defaultTip()
	}
	return r.resolveRef(ref)
}

func handleGetContents(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	ref := r.URL.Query().Get("ref")
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	if repo == nil {
		st.mu.RUnlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	tip, ok := repo.contentsTip(ref)
	if !ok {
		st.mu.RUnlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	treeOID := repo.commitTree(tip)
	if path == "" {
		// root listing
		out := dirListing(repo, treeOID, "")
		st.mu.RUnlock()
		writeJSON(w, 200, out)
		return
	}
	flat := map[string]treeEntry{}
	repo.flattenTree(treeOID, "", flat)
	if e, ok := flat[path]; ok {
		obj := repo.objects[e.oid]
		if obj == nil {
			st.mu.RUnlock()
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		b64 := base64.StdEncoding.EncodeToString(obj.data)
		st.mu.RUnlock()
		writeJSON(w, 200, contentJSON{
			Type: "file", Encoding: "base64", Content: b64, SHA: e.oid,
			Size: int64(len(obj.data)), Name: baseName(path), Path: path,
		})
		return
	}
	// directory?
	prefix := path + "/"
	var out []contentJSON
	for p, e := range flat {
		if strings.HasPrefix(p, prefix) {
			rest := p[len(prefix):]
			if strings.Contains(rest, "/") {
				continue
			}
			out = append(out, contentJSON{Type: "file", SHA: e.oid, Name: rest, Path: p})
		}
	}
	// subdirectories
	seenDirs := map[string]bool{}
	for p := range flat {
		if strings.HasPrefix(p, prefix) {
			rest := p[len(prefix):]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				seenDirs[rest[:i]] = true
			}
		}
	}
	for d := range seenDirs {
		// tree oid of the directory
		treeOID2 := dirTreeOID(repo, treeOID, path+"/"+d)
		out = append(out, contentJSON{Type: "dir", SHA: treeOID2, Name: d, Path: path + "/" + d})
	}
	st.mu.RUnlock()
	if out == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, out)
}

func dirTreeOID(repo *repo, treeOID, dirPath string) string {
	parts := strings.Split(dirPath, "/")
	cur := treeOID
	for _, part := range parts {
		obj, ok := repo.objects[cur]
		if !ok || obj.typ != objTree {
			return ""
		}
		found := ""
		for _, e := range parseTree(obj.data) {
			if e.name == part && e.mode == "40000" {
				found = e.oid
				break
			}
		}
		if found == "" {
			return ""
		}
		cur = found
	}
	return cur
}

func dirListing(repo *repo, treeOID, prefix string) []contentJSON {
	obj, ok := repo.objects[treeOID]
	if !ok || obj.typ != objTree {
		return []contentJSON{}
	}
	var out []contentJSON
	for _, e := range parseTree(obj.data) {
		p := prefix + e.name
		if e.mode == "40000" {
			out = append(out, contentJSON{Type: "dir", SHA: e.oid, Name: e.name, Path: p})
		} else {
			out = append(out, contentJSON{Type: "file", SHA: e.oid, Name: e.name, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func handlePutContents(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	if path == "" {
		writeJSON(w, 422, errJSON("path is required"))
		return
	}
	body, code := readJSONBody(r)
	if code != 0 {
		writeJSON(w, code, errJSON("Invalid JSON"))
		return
	}
	contentB64 := strField(body, "content")
	message := strField(body, "message")
	branch := strField(body, "branch")
	sha := strField(body, "sha")
	raw, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		writeJSON(w, 422, errJSON("content must be base64"))
		return
	}
	st.mu.Lock()
	repo := st.lookupRepoLocked(owner, name)
	if repo == nil {
		st.mu.Unlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	// resolve the branch tip (or create the branch from the default tip)
	var tip string
	var hasTip bool
	branchRef := ""
	if branch != "" {
		branchRef = "refs/heads/" + branch
		tip, hasTip = repo.refs[branchRef]
		if !hasTip {
			// create the branch from the default tip
			tip, hasTip = repo.defaultTip()
			if hasTip {
				repo.refs[branchRef] = tip
			}
		}
	} else {
		tip, hasTip = repo.defaultTip()
		branchRef = "refs/heads/main"
	}
	files := map[string]string{}
	if hasTip {
		files = repo.treeToFiles(repo.commitTree(tip))
	}
	blobOID := gitOID(objBlob, raw)
	if _, ok := repo.objects[blobOID]; !ok {
		repo.objects[blobOID] = &gitObject{typ: objBlob, data: raw}
	}
	_, exists := files[path]
	if exists {
		if sha == "" || sha != files[path] {
			st.mu.Unlock()
			writeJSON(w, 409, errJSON("sha does not match"))
			return
		}
	}
	files[path] = blobOID
	treeOID := repo.buildTreeLocked(files)
	parent := ""
	if hasTip {
		parent = tip
	}
	commitOID := repo.buildCommitLocked(parent, treeOID, message)
	repo.refs[branchRef] = commitOID
	repo.version++
	st.mu.Unlock()
	status := 201
	if exists {
		status = 200
	}
	writeJSON(w, status, map[string]any{
		"content": contentJSON{
			Type: "file", Encoding: "base64", Content: contentB64, SHA: blobOID,
			Size: int64(len(raw)), Name: baseName(path), Path: path,
		},
		"commit": map[string]string{"sha": commitOID},
	})
}

func handleDeleteContents(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	if path == "" {
		writeJSON(w, 422, errJSON("path is required"))
		return
	}
	body, code := readJSONBody(r)
	if code != 0 {
		writeJSON(w, code, errJSON("Invalid JSON"))
		return
	}
	message := strField(body, "message")
	branch := strField(body, "branch")
	sha := strField(body, "sha")
	st.mu.Lock()
	repo := st.lookupRepoLocked(owner, name)
	if repo == nil {
		st.mu.Unlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	branchRef := "refs/heads/main"
	if branch != "" {
		branchRef = "refs/heads/" + branch
	}
	tip, hasTip := repo.refs[branchRef]
	if !hasTip {
		st.mu.Unlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	files := repo.treeToFiles(repo.commitTree(tip))
	cur, exists := files[path]
	if !exists {
		st.mu.Unlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if sha == "" || sha != cur {
		st.mu.Unlock()
		writeJSON(w, 409, errJSON("sha does not match"))
		return
	}
	delete(files, path)
	treeOID := repo.buildTreeLocked(files)
	commitOID := repo.buildCommitLocked(tip, treeOID, message)
	repo.refs[branchRef] = commitOID
	repo.version++
	st.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"commit": map[string]string{"sha": commitOID},
	})
}

// ---------------------------------------------------------------------------
// PR files diff
// ---------------------------------------------------------------------------

type prFileJSON struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func prFiles(repo *repo, pr *pullRequest) []prFileJSON {
	baseTip, ok := repo.refs["refs/heads/"+pr.baseRef]
	if !ok {
		baseTip = pr.baseSHA
	}
	mb := repo.mergeBase(baseTip, pr.headSHA)
	baseTree := ""
	if mb != "" {
		baseTree = repo.commitTree(mb)
	}
	headTree := repo.commitTree(pr.headSHA)
	baseFlat := map[string]treeEntry{}
	headFlat := map[string]treeEntry{}
	repo.flattenTree(baseTree, "", baseFlat)
	repo.flattenTree(headTree, "", headFlat)
	var out []prFileJSON
	seen := map[string]bool{}
	for path, h := range headFlat {
		seen[path] = true
		b, inBase := baseFlat[path]
		if !inBase {
			data := repo.objects[h.oid].data
			add := countLines(data)
			out = append(out, prFileJSON{Filename: path, Status: "added", Additions: add, Deletions: 0, Changes: add})
			continue
		}
		if b.oid == h.oid {
			continue
		}
		oldData := repo.objects[b.oid].data
		newData := repo.objects[h.oid].data
		add, del := lineDiff(oldData, newData)
		out = append(out, prFileJSON{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
	}
	for path, b := range baseFlat {
		if seen[path] {
			continue
		}
		data := repo.objects[b.oid].data
		del := countLines(data)
		out = append(out, prFileJSON{Filename: path, Status: "removed", Additions: 0, Deletions: del, Changes: del})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	if out == nil {
		out = []prFileJSON{}
	}
	return out
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff counts added/removed lines via a multiset difference.
func lineDiff(oldData, newData []byte) (add, del int) {
	oldLines := map[string]int{}
	for _, l := range strings.Split(string(oldData), "\n") {
		oldLines[l]++
	}
	newLines := map[string]int{}
	for _, l := range strings.Split(string(newData), "\n") {
		newLines[l]++
	}
	for l, n := range newLines {
		if d := n - oldLines[l]; d > 0 {
			add += d
		}
	}
	for l, n := range oldLines {
		if d := n - newLines[l]; d > 0 {
			del += d
		}
	}
	return add, del
}
