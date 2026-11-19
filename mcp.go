package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// ---------------------------------------------------------------------------
// MCP (JSON-RPC 2.0, stateless, application/json)
// ---------------------------------------------------------------------------

var mcpToolsJSON []byte
var mcpToolsOnce sync.Once

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTools() []mcpToolDef {
	return []mcpToolDef{
		{
			Name:        "create_repo",
			Description: "Create a new repository for the viewer.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "Repository name"},
					"description": map[string]any{"type": "string", "description": "Repository description"},
					"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "Repository owner login"},
					"repo":  map[string]any{"type": "string", "description": "Repository name"},
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "Repository owner login"},
					"repo":  map[string]any{"type": "string", "description": "Repository name"},
					"title": map[string]any{"type": "string", "description": "Issue title"},
					"body":  map[string]any{"type": "string", "description": "Issue body"},
				},
				"required": []string{"owner", "repo", "title"},
			},
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "Repository owner login"},
					"repo":  map[string]any{"type": "string", "description": "Repository name"},
					"state": map[string]any{"type": "string", "description": "Issue state filter: open, closed, or all"},
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "Repository owner login"},
					"repo":  map[string]any{"type": "string", "description": "Repository name"},
					"title": map[string]any{"type": "string", "description": "Pull request title"},
					"head":  map[string]any{"type": "string", "description": "Head branch name"},
					"base":  map[string]any{"type": "string", "description": "Base branch name"},
				},
				"required": []string{"owner", "repo", "title", "head", "base"},
			},
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner":       map[string]any{"type": "string", "description": "Repository owner login"},
					"repo":        map[string]any{"type": "string", "description": "Repository name"},
					"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
				},
				"required": []string{"owner", "repo", "pull_number"},
			},
		},
	}
}

func mcpToolsBytes() []byte {
	mcpToolsOnce.Do(func() {
		b, _ := json.Marshal(mcpTools())
		mcpToolsJSON = b
	})
	return mcpToolsJSON
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		writeMCPErrorStatus(w, 400, nil, -32700, "Parse error")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeMCPErrorStatus(w, 400, nil, -32700, "Parse error")
		return
	}
	id := req["id"]
	method, _ := req["method"].(string)
	if method == "" {
		writeMCPError(w, id, -32600, "Invalid Request")
		return
	}
	switch method {
	case "notifications/initialized", "notifications/cancelled":
		// notification: 202 empty
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(202)
		return
	case "server/discover":
		writeMCPResult(w, id, map[string]any{
			"serverInfo":      map[string]any{"name": "gitforge", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"},
		})
	case "initialize":
		writeMCPResult(w, id, map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gitforge", "version": "1.0.0"},
		})
	case "ping":
		writeMCPResult(w, id, map[string]any{})
	case "tools/list":
		// byte-identical tools array every call
		var toolsRaw json.RawMessage = mcpToolsBytes()
		writeMCPResultRaw(w, id, map[string]any{"tools": toolsRaw})
	case "tools/call":
		params, _ := req["params"].(map[string]any)
		handleMCPCall(w, id, params)
	default:
		writeMCPError(w, id, -32601, "Method not found")
	}
}

func writeMCPResult(w http.ResponseWriter, id any, result any) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	writeJSON(w, 200, resp)
}

func writeMCPResultRaw(w http.ResponseWriter, id any, result map[string]any) {
	// marshal manually to keep the precomputed tools bytes intact
	toolsRaw := result["tools"].(json.RawMessage)
	idJSON, _ := json.Marshal(id)
	var buf []byte
	buf = append(buf, []byte(`{"jsonrpc":"2.0","id":`)...)
	buf = append(buf, idJSON...)
	buf = append(buf, []byte(`,"result":{"tools":`)...)
	buf = append(buf, toolsRaw...)
	buf = append(buf, []byte(`}}`)...)
	writeBytes(w, 200, buf)
}

func writeMCPError(w http.ResponseWriter, id any, code int, msg string) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
	writeJSON(w, 200, resp)
}

func writeMCPErrorStatus(w http.ResponseWriter, status int, id any, code int, msg string) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
	writeJSON(w, status, resp)
}

func mcpTextResult(obj any, isErr bool) map[string]any {
	b, _ := json.Marshal(obj)
	res := map[string]any{
		"content":          []map[string]any{{"type": "text", "text": string(b)}},
		"structuredContent": obj,
	}
	if isErr {
		res["isError"] = true
	}
	return res
}

func handleMCPCall(w http.ResponseWriter, id any, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	if name == "" {
		writeMCPError(w, id, -32602, "tool name is required")
		return
	}
	// argument synonyms: the suite maps semantic args onto schema names;
	// accept a few common aliases.
	normalizeMCPArgs(name, args)
	switch name {
	case "create_repo":
		rname, _ := args["name"].(string)
		if rname == "" {
			writeMCPResult(w, id, mcpTextResult("name is required", true))
			return
		}
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		st.mu.Lock()
		repo, err := st.createRepoLocked(rname, desc, priv)
		st.mu.Unlock()
		if err != nil {
			writeMCPResult(w, id, mcpTextResult("Repository already exists", true))
			return
		}
		writeMCPResult(w, id, mcpTextResult(repo.toJSON(), false))
	case "get_repo":
		owner, _ := args["owner"].(string)
		rname, _ := args["repo"].(string)
		if owner == "" || rname == "" {
			writeMCPResult(w, id, mcpTextResult("owner and repo are required", true))
			return
		}
		st.mu.RLock()
		repo := st.lookupRepoLocked(owner, rname)
		st.mu.RUnlock()
		if repo == nil {
			writeMCPResult(w, id, mcpTextResult("Not Found", true))
			return
		}
		writeMCPResult(w, id, mcpTextResult(repo.toJSON(), false))
	case "create_issue":
		owner, _ := args["owner"].(string)
		rname, _ := args["repo"].(string)
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		if owner == "" || rname == "" || title == "" {
			writeMCPResult(w, id, mcpTextResult("owner, repo and title are required", true))
			return
		}
		st.mu.Lock()
		repo := st.lookupRepoLocked(owner, rname)
		if repo == nil {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("Not Found", true))
			return
		}
		st.issSeq++
		num := int64(len(repo.issues)) + 1
		is := &issue{
			id:        st.issSeq,
			number:    num,
			title:     title,
			body:      body,
			state:     "open",
			labels:    []label{},
			createdAt: stamp(st.nextClock()),
			updatedAt: stamp(st.clock - 1),
		}
		is.refreshCache(repo)
		repo.issues = append(repo.issues, is)
		repo.issueByN[num] = is
		st.mu.Unlock()
		writeMCPResult(w, id, mcpTextResult(is.toJSON(repo), false))
	case "list_issues":
		owner, _ := args["owner"].(string)
		rname, _ := args["repo"].(string)
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		if owner == "" || rname == "" {
			writeMCPResult(w, id, mcpTextResult("owner and repo are required", true))
			return
		}
		st.mu.RLock()
		repo := st.lookupRepoLocked(owner, rname)
		if repo == nil {
			st.mu.RUnlock()
			writeMCPResult(w, id, mcpTextResult("Not Found", true))
			return
		}
		var out []issueJSON
		for _, is := range repo.issues {
			if state == "all" || is.state == state {
				out = append(out, is.toJSON(repo))
			}
		}
		st.mu.RUnlock()
		if out == nil {
			out = []issueJSON{}
		}
		writeMCPResult(w, id, mcpTextResult(out, false))
	case "create_pull_request":
		owner, _ := args["owner"].(string)
		rname, _ := args["repo"].(string)
		title, _ := args["title"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		if owner == "" || rname == "" || title == "" || head == "" || base == "" {
			writeMCPResult(w, id, mcpTextResult("owner, repo, title, head and base are required", true))
			return
		}
		st.mu.Lock()
		repo := st.lookupRepoLocked(owner, rname)
		if repo == nil {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("Not Found", true))
			return
		}
		headSHA, okH := repo.resolveRef(head)
		baseSHA, okB := repo.resolveRef(base)
		if !okH || !okB {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("head or base ref does not exist", true))
			return
		}
		st.prSeq++
		pr := &pullRequest{
			id:        st.prSeq,
			number:    int64(len(repo.prs)) + 1,
			title:     title,
			state:     "open",
			headRef:   head,
			headSHA:   headSHA,
			baseRef:   base,
			baseSHA:   baseSHA,
			createdAt: stamp(st.nextClock()),
		}
		repo.prs = append(repo.prs, pr)
		repo.prByN[pr.number] = pr
		st.mu.Unlock()
		out := pr.toJSON(repo)
		// include pull_number alias for the harness
		m := map[string]any{}
		b, _ := json.Marshal(out)
		json.Unmarshal(b, &m)
		m["pull_number"] = pr.number
		writeMCPResult(w, id, mcpTextResult(m, false))
	case "merge_pull_request":
		owner, _ := args["owner"].(string)
		rname, _ := args["repo"].(string)
		num := int64(0)
		switch v := args["pull_number"].(type) {
		case float64:
			num = int64(v)
		case int64:
			num = v
		case string:
			num, _ = strconv.ParseInt(v, 10, 64)
		}
		if owner == "" || rname == "" || num == 0 {
			writeMCPResult(w, id, mcpTextResult("owner, repo and pull_number are required", true))
			return
		}
		st.mu.Lock()
		repo := st.lookupRepoLocked(owner, rname)
		if repo == nil {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("Not Found", true))
			return
		}
		pr := repo.prByN[num]
		if pr == nil {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("Pull request not found", true))
			return
		}
		if pr.merged {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("Pull request already merged", true))
			return
		}
		baseTip, ok := repo.refs["refs/heads/"+pr.baseRef]
		if !ok {
			st.mu.Unlock()
			writeMCPResult(w, id, mcpTextResult("base ref not found", true))
			return
		}
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
		writeMCPResult(w, id, mcpTextResult(map[string]any{
			"merged": true, "sha": commitOID, "number": num, "state": "closed",
		}, false))
	default:
		writeMCPError(w, id, -32602, fmt.Sprintf("unknown tool %q", name))
	}
}

// normalizeMCPArgs maps common argument synonyms onto the schema names.
func normalizeMCPArgs(tool string, args map[string]any) {
	alias := map[string]string{
		"repository": "repo",
		"repo_name":  "repo",
		"number":     "pull_number",
		"pr_number":  "pull_number",
		"head_ref":   "head",
		"base_ref":   "base",
	}
	for from, to := range alias {
		if v, ok := args[from]; ok {
			if _, exists := args[to]; !exists {
				args[to] = v
			}
		}
	}
}
