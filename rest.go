package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

var reqSeq int64

func nextReqID() string {
	return "forge-" + strconv.FormatInt(atomic.AddInt64(&reqSeq, 1), 10)
}

func errJSON(msg string) map[string]string { return map[string]string{"message": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(status)
	b, _ := json.Marshal(v)
	w.Write(b)
}

func writeBytes(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(status)
	w.Write(b)
}

func readJSONBody(r *http.Request) (map[string]any, int) {
	if r.ContentLength == 0 {
		return map[string]any{}, 0
	}
	b, err := io.ReadAll(r.Body)
	if err != nil || len(b) == 0 {
		return map[string]any{}, 0
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, 400
	}
	return m, 0
}

func strField(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, k string, def bool) bool {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func intField(m map[string]any, k string, def int) int {
	if v, ok := m[k]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return def
}

var shopToken = ""

func authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	if tok == "" {
		return false
	}
	if shopToken == "" {
		return true
	}
	return tok == shopToken
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

func handler(w http.ResponseWriter, r *http.Request) {
	// The conformance client (undici) reuses keep-alive connections only when
	// the server says so explicitly; without it every request pays a new
	// TCP handshake.
	w.Header().Set("Connection", "keep-alive")
	// v6 sends no Date header and the checks do not require one; suppress
	// Go's per-response Date generation (a time.Now + format per reply).
	w.Header()["Date"] = nil
	p := r.URL.Path
	// health
	if p == "/healthz" {
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(200)
		return
	}
	// git smart http
	if strings.Contains(p, ".git/") {
		switch {
		case strings.HasSuffix(p, "/info/refs") && r.Method == "GET":
			handleInfoRefs(w, r)
		case strings.HasSuffix(p, "/git-upload-pack") && r.Method == "POST":
			handleUploadPack(w, r)
		case strings.HasSuffix(p, "/git-receive-pack") && r.Method == "POST":
			handleReceivePack(w, r)
		default:
			writeJSON(w, 404, errJSON("Not Found"))
		}
		return
	}
	// graphql / mcp (auth required)
	if p == "/api/graphql" {
		if r.Method != "POST" {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		if !authorized(r) {
			writeJSON(w, 401, errJSON("Requires authentication"))
			return
		}
		handleGraphQL(w, r)
		return
	}
	if p == "/mcp" {
		if r.Method == "DELETE" {
			w.Header().Set("X-GitHub-Request-Id", nextReqID())
			w.WriteHeader(405)
			return
		}
		if r.Method != "POST" {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		if !authorized(r) {
			writeJSON(w, 401, errJSON("Requires authentication"))
			return
		}
		handleMCP(w, r)
		return
	}
	// REST v3
	if p == "/api/v3/" || p == "/api/v3" {
		if r.Method != "GET" {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, 200, map[string]string{
			"current_user_url": "/api/v3/user",
			"repository_url":   "/api/v3/repos/{owner}/{repo}",
		})
		return
	}
	if !strings.HasPrefix(p, "/api/v3/") {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if !authorized(r) {
		writeJSON(w, 401, errJSON("Requires authentication"))
		return
	}
	restDispatch(w, r, strings.TrimPrefix(p, "/api/v3/"))
}

func restDispatch(w http.ResponseWriter, r *http.Request, rest string) {
	seg := strings.Split(strings.Trim(rest, "/"), "/")
	switch seg[0] {
	case "meta":
		if r.Method == "GET" && len(seg) == 1 {
			writeJSON(w, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": true})
			return
		}
	case "user":
		if len(seg) == 1 && r.Method == "GET" {
			writeJSON(w, 200, map[string]any{
				"login": viewerLogin, "id": viewerID, "name": viewerName,
				"email": viewerEmail, "type": "User",
			})
			return
		}
		if len(seg) == 2 && seg[1] == "repos" {
			if r.Method == "GET" {
				handleListUserRepos(w, r)
				return
			}
			if r.Method == "POST" {
				handleCreateRepo(w, r)
				return
			}
		}
	case "repos":
		if len(seg) >= 3 {
			handleRepoRoutes(w, r, seg[1], seg[2], seg[3:])
			return
		}
	}
	writeJSON(w, 404, errJSON("Not Found"))
}

func handleListUserRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := intField(map[string]any{"per_page": q.Get("per_page")}, "per_page", 30)
	// parse per_page/page from query directly (they are strings)
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		perPage = v
	}
	page := 1
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	st.mu.RLock()
	names := make([]string, 0, len(st.repos))
	for n := range st.repos {
		names = append(names, n)
	}
	st.mu.RUnlock()
	// creation order = id order
	sort.Slice(names, func(i, j int) bool {
		st.mu.RLock()
		defer st.mu.RUnlock()
		return st.repos[names[i]].id < st.repos[names[j]].id
	})
	total := len(names)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var out []repoJSON
	st.mu.RLock()
	for _, n := range names[start:end] {
		out = append(out, st.repos[n].toJSON())
	}
	st.mu.RUnlock()
	if out == nil {
		out = []repoJSON{}
	}
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	base := "http://" + r.Host + "/api/v3/user/repos"
	var links []string
	if page < totalPages {
		links = append(links, fmt.Sprintf("<%s?per_page=%d&page=%d>; rel=\"next\"", base, perPage, page+1))
		links = append(links, fmt.Sprintf("<%s?per_page=%d&page=%d>; rel=\"last\"", base, perPage, totalPages))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	writeJSON(w, 200, out)
}

func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	body, code := readJSONBody(r)
	if code != 0 {
		writeJSON(w, code, errJSON("Invalid JSON"))
		return
	}
	name := strField(body, "name")
	if name == "" {
		writeJSON(w, 422, errJSON("name is required"))
		return
	}
	st.mu.Lock()
	repo, err := st.createRepoLocked(name, strField(body, "description"), boolField(body, "private", false))
	st.mu.Unlock()
	if err != nil {
		writeJSON(w, 422, errJSON("Repository already exists"))
		return
	}
	writeJSON(w, 201, repo.toJSON())
}

func handleRepoRoutes(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	// tail is everything after /repos/{owner}/{repo}
	if len(tail) == 0 {
		switch r.Method {
		case "GET":
			st.mu.RLock()
			repo := st.lookupRepoLocked(owner, name)
			st.mu.RUnlock()
			if repo == nil {
				writeJSON(w, 404, errJSON("Not Found"))
				return
			}
			writeJSON(w, 200, repo.toJSON())
		case "PATCH":
			body, code := readJSONBody(r)
			if code != 0 {
				writeJSON(w, code, errJSON("Invalid JSON"))
				return
			}
			st.mu.Lock()
			repo := st.lookupRepoLocked(owner, name)
			if repo == nil {
				st.mu.Unlock()
				writeJSON(w, 404, errJSON("Not Found"))
				return
			}
			if d, ok := body["description"]; ok {
				if s, ok := d.(string); ok {
					repo.description = s
				}
			}
			st.mu.Unlock()
			writeJSON(w, 200, repo.toJSON())
		case "DELETE":
			st.mu.Lock()
			ok := st.deleteRepoLocked(name)
			st.mu.Unlock()
			if !ok || owner != viewerLogin {
				writeJSON(w, 404, errJSON("Not Found"))
				return
			}
			w.Header().Set("X-GitHub-Request-Id", nextReqID())
			w.WriteHeader(204)
		default:
			writeJSON(w, 404, errJSON("Not Found"))
		}
		return
	}
	switch tail[0] {
	case "branches":
		handleBranches(w, r, owner, name, tail[1:])
	case "commits":
		if r.Method == "GET" && len(tail) == 1 {
			handleListCommits(w, r, owner, name)
			return
		}
		writeJSON(w, 404, errJSON("Not Found"))
	case "git":
		handleGitRoutes(w, r, owner, name, tail[1:])
	case "labels":
		handleLabels(w, r, owner, name, tail[1:])
	case "issues":
		handleIssues(w, r, owner, name, tail[1:])
	case "contents":
		handleContents(w, r, owner, name, tail[1:])
	case "pulls":
		handlePulls(w, r, owner, name, tail[1:])
	default:
		writeJSON(w, 404, errJSON("Not Found"))
	}
}

func handleBranches(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	if r.Method != "GET" {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	if repo == nil {
		st.mu.RUnlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if len(tail) == 0 {
		var out []branchJSON
		for _, rn := range repo.sortedRefNames() {
			if !strings.HasPrefix(rn, "refs/heads/") {
				continue
			}
			var b branchJSON
			b.Name = strings.TrimPrefix(rn, "refs/heads/")
			b.Commit.SHA = repo.refs[rn]
			out = append(out, b)
		}
		st.mu.RUnlock()
		if out == nil {
			out = []branchJSON{}
		}
		writeJSON(w, 200, out)
		return
	}
	branch := strings.Join(tail, "/")
	oid, ok := repo.resolveRef(branch)
	st.mu.RUnlock()
	if !ok {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	var b branchJSON
	b.Name = branch
	b.Commit.SHA = oid
	writeJSON(w, 200, b)
}

func handleListCommits(w http.ResponseWriter, r *http.Request, owner, name string) {
	sha := r.URL.Query().Get("sha")
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	if repo == nil {
		st.mu.RUnlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	var tip string
	if sha == "" {
		t, ok := repo.defaultTip()
		if !ok {
			st.mu.RUnlock()
			writeJSON(w, 200, []any{})
			return
		}
		tip = t
	} else {
		t, ok := repo.resolveRef(sha)
		if !ok {
			// maybe a raw commit oid
			if _, ok := repo.objects[sha]; ok {
				tip = sha
			} else {
				st.mu.RUnlock()
				writeJSON(w, 404, errJSON("Not Found"))
				return
			}
		} else {
			tip = t
		}
	}
	chain := repo.commitChain(tip, 1000)
	type commitOut struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	out := make([]commitOut, 0, len(chain))
	for _, oid := range chain {
		obj := repo.objects[oid]
		if obj == nil {
			continue
		}
		var c commitOut
		c.SHA = oid
		c.Commit.Message = commitMessage(obj.data)
		out = append(out, c)
	}
	st.mu.RUnlock()
	writeJSON(w, 200, out)
}

func handleGitRoutes(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	if len(tail) == 0 || tail[0] != "refs" {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if r.Method == "POST" && len(tail) == 1 {
		body, code := readJSONBody(r)
		if code != 0 {
			writeJSON(w, code, errJSON("Invalid JSON"))
			return
		}
		ref := strField(body, "ref")
		sha := strField(body, "sha")
		if ref == "" || sha == "" {
			writeJSON(w, 422, errJSON("ref and sha are required"))
			return
		}
		st.mu.Lock()
		repo := st.lookupRepoLocked(owner, name)
		if repo == nil {
			st.mu.Unlock()
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		if _, exists := repo.refs[ref]; exists {
			st.mu.Unlock()
			writeJSON(w, 422, errJSON("Reference already exists"))
			return
		}
		if _, ok := repo.objects[sha]; !ok {
			st.mu.Unlock()
			writeJSON(w, 422, errJSON("Object does not exist"))
			return
		}
		repo.refs[ref] = sha
		repo.version++
		st.mu.Unlock()
		writeJSON(w, 201, refJSON{Ref: ref, Object: refObjectJSON{SHA: sha, Type: "commit"}})
		return
	}
	if r.Method == "GET" {
		st.mu.RLock()
		repo := st.lookupRepoLocked(owner, name)
		if repo == nil {
			st.mu.RUnlock()
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		if len(tail) == 1 {
			var out []refJSON
			for _, rn := range repo.sortedRefNames() {
				out = append(out, refJSON{Ref: rn, Object: refObjectJSON{SHA: repo.refs[rn], Type: "commit"}})
			}
			st.mu.RUnlock()
			if out == nil {
				out = []refJSON{}
			}
			writeJSON(w, 200, out)
			return
		}
		refName := strings.Join(tail[1:], "/")
		oid, ok := repo.refs[refName]
		st.mu.RUnlock()
		if !ok {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		writeJSON(w, 200, refJSON{Ref: refName, Object: refObjectJSON{SHA: oid, Type: "commit"}})
		return
	}
	writeJSON(w, 404, errJSON("Not Found"))
}

func handleLabels(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	st.mu.RUnlock()
	if repo == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	switch r.Method {
	case "GET":
		if len(tail) != 0 {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		st.mu.RLock()
		var out []labelJSON
		names := make([]string, 0, len(repo.labels))
		for n := range repo.labels {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, labelJSON{Name: repo.labels[n].name, Color: repo.labels[n].color})
		}
		st.mu.RUnlock()
		if out == nil {
			out = []labelJSON{}
		}
		writeJSON(w, 200, out)
	case "POST":
		body, code := readJSONBody(r)
		if code != 0 {
			writeJSON(w, code, errJSON("Invalid JSON"))
			return
		}
		lname := strField(body, "name")
		color := strField(body, "color")
		if lname == "" {
			writeJSON(w, 422, errJSON("name is required"))
			return
		}
		if color == "" {
			color = "ededed"
		}
		st.mu.Lock()
		lb, ok := repo.labels[lname]
		if !ok {
			lb = &label{name: lname, color: color}
			repo.labels[lname] = lb
		}
		st.mu.Unlock()
		writeJSON(w, 201, labelJSON{Name: lb.name, Color: lb.color})
	default:
		writeJSON(w, 404, errJSON("Not Found"))
	}
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func handleIssues(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	st.mu.RUnlock()
	if repo == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if len(tail) == 0 {
		switch r.Method {
		case "GET":
			handleListIssues(w, r, repo)
		case "POST":
			handleCreateIssue(w, r, repo)
		default:
			writeJSON(w, 404, errJSON("Not Found"))
		}
		return
	}
	num, err := strconv.ParseInt(tail[0], 10, 64)
	if err != nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		switch r.Method {
		case "GET":
			st.mu.RLock()
			cs := repo.comments[num]
			st.mu.RUnlock()
			var out []commentJSON
			for _, c := range cs {
				out = append(out, commentJSON{ID: c.id, Body: c.body, User: viewerUser(), CreatedAt: c.createdAt})
			}
			if out == nil {
				out = []commentJSON{}
			}
			writeJSON(w, 200, out)
		case "POST":
			body, code := readJSONBody(r)
			if code != 0 {
				writeJSON(w, code, errJSON("Invalid JSON"))
				return
			}
			st.mu.Lock()
			is := repo.issueByN[num]
			if is == nil {
				st.mu.Unlock()
				writeJSON(w, 404, errJSON("Not Found"))
				return
			}
			st.cmtSeq++
			c := &comment{id: st.cmtSeq, body: strField(body, "body"), createdAt: stamp(st.nextClock())}
			repo.comments[num] = append(repo.comments[num], c)
			st.mu.Unlock()
			writeJSON(w, 201, commentJSON{ID: c.id, Body: c.body, User: viewerUser(), CreatedAt: c.createdAt})
		default:
			writeJSON(w, 404, errJSON("Not Found"))
		}
		return
	}
	if len(tail) != 1 {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	switch r.Method {
	case "GET":
		st.mu.RLock()
		is := repo.issueByN[num]
		st.mu.RUnlock()
		if is == nil {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		// hot path: serve pre-marshaled bytes
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(200)
		w.Write(is.cached)
	case "PATCH":
		body, code := readJSONBody(r)
		if code != 0 {
			writeJSON(w, code, errJSON("Invalid JSON"))
			return
		}
		st.mu.Lock()
		is := repo.issueByN[num]
		if is == nil {
			st.mu.Unlock()
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		if v, ok := body["state"]; ok {
			if s, ok := v.(string); ok && (s == "open" || s == "closed") {
				is.state = s
			}
		}
		if v, ok := body["title"]; ok {
			if s, ok := v.(string); ok {
				is.title = s
			}
		}
		if v, ok := body["body"]; ok {
			if s, ok := v.(string); ok {
				is.body = s
			}
		}
		is.refreshCache(repo)
		st.mu.Unlock()
		writeJSON(w, 200, is.toJSON(repo))
	default:
		writeJSON(w, 404, errJSON("Not Found"))
	}
}

func handleListIssues(w http.ResponseWriter, r *http.Request, repo *repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortParam := q.Get("sort")
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := 30
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		perPage = v
	}
	page := 1
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	st.mu.RLock()
	var list []*issue
	for _, is := range repo.issues {
		if state == "all" || is.state == state {
			list = append(list, is)
		}
	}
	st.mu.RUnlock()
	// sort by creation (number); sort param is only "created" here
	if sortParam == "created" || sortParam == "" {
		if direction == "asc" {
			sort.Slice(list, func(i, j int) bool { return list[i].number < list[j].number })
		} else {
			sort.Slice(list, func(i, j int) bool { return list[i].number > list[j].number })
		}
	}
	total := len(list)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var out []issueJSON
	for _, is := range list[start:end] {
		out = append(out, is.toJSON(repo))
	}
	if out == nil {
		out = []issueJSON{}
	}
	writeJSON(w, 200, out)
}

func handleCreateIssue(w http.ResponseWriter, r *http.Request, repo *repo) {
	body, code := readJSONBody(r)
	if code != 0 {
		writeJSON(w, code, errJSON("Invalid JSON"))
		return
	}
	title := strField(body, "title")
	if title == "" {
		writeJSON(w, 422, errJSON("title is required"))
		return
	}
	var labelNames []string
	if v, ok := body["labels"]; ok {
		if arr, ok := v.([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok {
					labelNames = append(labelNames, s)
				} else if m, ok := e.(map[string]any); ok {
					if s, ok := m["name"].(string); ok {
						labelNames = append(labelNames, s)
					}
				}
			}
		}
	}
	st.mu.Lock()
	for _, ln := range labelNames {
		if _, ok := repo.labels[ln]; !ok {
			st.mu.Unlock()
			writeJSON(w, 422, errJSON("Label does not exist"))
			return
		}
	}
	st.issSeq++
	num := int64(len(repo.issues)) + 1
	is := &issue{
		id:        st.issSeq,
		number:    num,
		title:     title,
		body:      strField(body, "body"),
		state:     "open",
		createdAt: stamp(st.nextClock()),
		updatedAt: stamp(st.clock - 1),
	}
	for _, ln := range labelNames {
		is.labels = append(is.labels, *repo.labels[ln])
	}
	if is.labels == nil {
		is.labels = []label{}
	}
	is.refreshCache(repo)
	repo.issues = append(repo.issues, is)
	repo.issueByN[num] = is
	st.mu.Unlock()
	// the create response is exactly the pre-marshaled issue JSON: no second marshal
	writeBytes(w, 201, is.cached)
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

func handlePulls(w http.ResponseWriter, r *http.Request, owner, name string, tail []string) {
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	st.mu.RUnlock()
	if repo == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if len(tail) == 0 {
		switch r.Method {
		case "GET":
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			st.mu.RLock()
			var out []prJSON
			for i := len(repo.prs) - 1; i >= 0; i-- {
				pr := repo.prs[i]
				if state == "all" || pr.state == state {
					out = append(out, pr.toJSON(repo))
				}
			}
			st.mu.RUnlock()
			if out == nil {
				out = []prJSON{}
			}
			writeJSON(w, 200, out)
		case "POST":
			handleCreatePR(w, r, repo)
		default:
			writeJSON(w, 404, errJSON("Not Found"))
		}
		return
	}
	num, err := strconv.ParseInt(tail[0], 10, 64)
	if err != nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if len(tail) == 2 && tail[1] == "files" && r.Method == "GET" {
		st.mu.RLock()
		pr := repo.prByN[num]
		if pr == nil {
			st.mu.RUnlock()
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		files := prFiles(repo, pr)
		st.mu.RUnlock()
		writeJSON(w, 200, files)
		return
	}
	if len(tail) == 2 && tail[1] == "merge" && r.Method == "PUT" {
		handleMergePR(w, r, repo, num)
		return
	}
	if len(tail) == 1 && r.Method == "GET" {
		st.mu.RLock()
		pr := repo.prByN[num]
		st.mu.RUnlock()
		if pr == nil {
			writeJSON(w, 404, errJSON("Not Found"))
			return
		}
		writeJSON(w, 200, pr.toJSON(repo))
		return
	}
	writeJSON(w, 404, errJSON("Not Found"))
}

func handleCreatePR(w http.ResponseWriter, r *http.Request, repo *repo) {
	body, code := readJSONBody(r)
	if code != 0 {
		writeJSON(w, code, errJSON("Invalid JSON"))
		return
	}
	headRef := strField(body, "head")
	baseRef := strField(body, "base")
	title := strField(body, "title")
	if headRef == "" || baseRef == "" {
		writeJSON(w, 422, errJSON("head and base are required"))
		return
	}
	st.mu.Lock()
	headSHA, okH := repo.resolveRef(headRef)
	baseSHA, okB := repo.resolveRef(baseRef)
	if !okH || !okB {
		st.mu.Unlock()
		writeJSON(w, 422, errJSON("head or base ref does not exist"))
		return
	}
	st.prSeq++
	pr := &pullRequest{
		id:        st.prSeq,
		number:    int64(len(repo.prs)) + 1,
		title:     title,
		body:      strField(body, "body"),
		state:     "open",
		headRef:   headRef,
		headSHA:   headSHA,
		baseRef:   baseRef,
		baseSHA:   baseSHA,
		createdAt: stamp(st.nextClock()),
	}
	repo.prs = append(repo.prs, pr)
	repo.prByN[pr.number] = pr
	st.mu.Unlock()
	writeJSON(w, 201, pr.toJSON(repo))
}

func handleMergePR(w http.ResponseWriter, r *http.Request, repo *repo, num int64) {
	body, code := readJSONBody(r)
	if code != 0 {
		writeJSON(w, code, errJSON("Invalid JSON"))
		return
	}
	_ = body
	st.mu.Lock()
	pr := repo.prByN[num]
	if pr == nil {
		st.mu.Unlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	if pr.merged {
		st.mu.Unlock()
		writeJSON(w, 405, errJSON("Pull request already merged"))
		return
	}
	// conflict: a path changed head-vs-merge-base AND base-tip-vs-merge-base
	// with differing blobs.
	baseTip, ok := repo.refs["refs/heads/"+pr.baseRef]
	if !ok {
		st.mu.Unlock()
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	mb := repo.mergeBase(baseTip, pr.headSHA)
	conflict := false
	if mb != "" {
		mbTree := repo.commitTree(mb)
		headTree := repo.commitTree(pr.headSHA)
		baseTree := repo.commitTree(baseTip)
		mbFlat := map[string]treeEntry{}
		headFlat := map[string]treeEntry{}
		baseFlat := map[string]treeEntry{}
		repo.flattenTree(mbTree, "", mbFlat)
		repo.flattenTree(headTree, "", headFlat)
		repo.flattenTree(baseTree, "", baseFlat)
		for path, h := range headFlat {
			m, inMB := mbFlat[path]
			b, inBase := baseFlat[path]
			if inMB && h.oid != m.oid && inBase && b.oid != m.oid && h.oid != b.oid {
				conflict = true
				break
			}
		}
		if !conflict {
			for path, b := range baseFlat {
				m, inMB := mbFlat[path]
				if _, inHead := headFlat[path]; !inHead && inMB && b.oid != m.oid {
					// deleted in head, changed in base
					conflict = true
					break
				}
			}
		}
	}
	if conflict {
		st.mu.Unlock()
		writeJSON(w, 405, errJSON("Merge conflict"))
		return
	}
	// squash merge: new commit on the base branch whose tree is the head tree
	headTree := repo.commitTree(pr.headSHA)
	mergeMsg := pr.title
	if mergeMsg == "" {
		mergeMsg = "Merge pull request"
	}
	commitOID := repo.buildCommitLocked(baseTip, headTree, mergeMsg)
	repo.refs["refs/heads/"+pr.baseRef] = commitOID
	repo.version++
	pr.merged = true
	pr.state = "closed"
	pr.mergedAt = stamp(st.nextClock())
	pr.mergeCommitSHA = commitOID
	st.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"sha":     commitOID,
		"merged":  true,
		"message": mergeMsg,
	})
}
