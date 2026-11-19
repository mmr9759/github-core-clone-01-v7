package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Minimal GraphQL: lexer, parser, executor for the documented subset.
// ---------------------------------------------------------------------------

type gqlToken struct {
	kind string // name, string, int, float, punct, var, eof
	val  string
}

func gqlLex(src string) ([]gqlToken, error) {
	var toks []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@':
			toks = append(toks, gqlToken{"punct", string(c)})
			i++
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			toks = append(toks, gqlToken{"var", src[i+1 : j]})
			i = j
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlToken{"punct", "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(src[j])
					}
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlToken{"string", sb.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlToken{kind, src[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			toks = append(toks, gqlToken{"name", src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlToken{"eof", ""})
	return toks, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

type gqlValue struct {
	kind string // var, string, int, float, bool, null, enum, list, object
	s    string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sel   []*gqlField // selection set; nil = scalar
	frag  string      // inline fragment type condition, "" if not a fragment
}

type gqlOperation struct {
	kind      string // query | mutation
	name      string
	varDefs   map[string]*gqlValue // variable name -> default value (nil if none)
	varTypes  map[string]string
	selection []*gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind, val string) (gqlToken, error) {
	t := p.next()
	if t.kind != kind || (val != "" && t.val != val) {
		return t, fmt.Errorf("expected %s %q, got %s %q", kind, val, t.kind, t.val)
	}
	return t, nil
}

func parseGraphQL(src string) (*gqlOperation, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	return p.parseOperation()
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", varDefs: map[string]*gqlValue{}, varTypes: map[string]string{}}
	t := p.peek()
	if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.kind = p.next().val
		if p.peek().kind == "name" {
			op.name = p.next().val
		}
	} else if t.kind == "punct" && t.val == "{" {
		// shorthand query
	} else {
		return nil, fmt.Errorf("expected operation")
	}
	// variable definitions
	if p.peek().kind == "punct" && p.peek().val == "(" {
		p.next()
		for {
			if p.peek().kind == "punct" && p.peek().val == ")" {
				p.next()
				break
			}
			vt, err := p.expect("var", "")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect("punct", ":"); err != nil {
				return nil, err
			}
			// type: name, possibly [..] and !
			typ, err := p.parseType()
			if err != nil {
				return nil, err
			}
			op.varTypes[vt.val] = typ
			if p.peek().kind == "punct" && p.peek().val == "=" {
				p.next()
				def, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				op.varDefs[vt.val] = def
			}
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selection = sel
	return op, nil
}

func (p *gqlParser) parseType() (string, error) {
	t := p.next()
	switch {
	case t.kind == "punct" && t.val == "[":
		inner, err := p.parseType()
		if err != nil {
			return "", err
		}
		if _, err := p.expect("punct", "]"); err != nil {
			return "", err
		}
		typ := "[" + inner + "]"
		if p.peek().kind == "punct" && p.peek().val == "!" {
			p.next()
			typ += "!"
		}
		return typ, nil
	case t.kind == "name":
		typ := t.val
		if p.peek().kind == "punct" && p.peek().val == "!" {
			p.next()
			typ += "!"
		}
		return typ, nil
	}
	return "", fmt.Errorf("bad type")
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if _, err := p.expect("punct", "{"); err != nil {
		return nil, err
	}
	var out []*gqlField
	for {
		if p.peek().kind == "punct" && p.peek().val == "}" {
			p.next()
			break
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	// inline fragment or spread
	if p.peek().kind == "punct" && p.peek().val == "..." {
		p.next()
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			tn, err := p.expect("name", "")
			if err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlField{frag: tn.val, sel: sel}, nil
		}
		// named fragment spread: unsupported
		_, err := p.expect("name", "")
		return nil, fmt.Errorf("fragment spreads unsupported: %v", err)
	}
	nameTok, err := p.expect("name", "")
	if err != nil {
		return nil, err
	}
	f := &gqlField{name: nameTok.val, args: map[string]*gqlValue{}}
	// alias
	if p.peek().kind == "punct" && p.peek().val == ":" {
		p.next()
		real, err := p.expect("name", "")
		if err != nil {
			return nil, err
		}
		f.alias = f.name
		f.name = real.val
	}
	// arguments
	if p.peek().kind == "punct" && p.peek().val == "(" {
		p.next()
		for {
			if p.peek().kind == "punct" && p.peek().val == ")" {
				p.next()
				break
			}
			argName, err := p.expect("name", "")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect("punct", ":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f.args[argName.val] = v
		}
	}
	// directives: skip @name[(args)]
	for p.peek().kind == "punct" && p.peek().val == "@" {
		p.next()
		if _, err := p.expect("name", ""); err != nil {
			return nil, err
		}
		if p.peek().kind == "punct" && p.peek().val == "(" {
			p.next()
			depth := 1
			for depth > 0 {
				t := p.next()
				if t.kind == "punct" && t.val == "(" {
					depth++
				} else if t.kind == "punct" && t.val == ")" {
					depth--
				} else if t.kind == "eof" {
					return nil, fmt.Errorf("unterminated directive")
				}
			}
		}
	}
	// sub-selection
	if p.peek().kind == "punct" && p.peek().val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch t.kind {
	case "var":
		return &gqlValue{kind: "var", s: t.val}, nil
	case "string":
		return &gqlValue{kind: "string", s: t.val}, nil
	case "int":
		return &gqlValue{kind: "int", s: t.val}, nil
	case "float":
		return &gqlValue{kind: "float", s: t.val}, nil
	case "name":
		switch t.val {
		case "true", "false":
			return &gqlValue{kind: "bool", s: t.val}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", s: t.val}, nil
		}
	case "punct":
		switch t.val {
		case "[":
			v := &gqlValue{kind: "list"}
			for {
				if p.peek().kind == "punct" && p.peek().val == "]" {
					p.next()
					break
				}
				e, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, e)
			}
			return v, nil
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for {
				if p.peek().kind == "punct" && p.peek().val == "}" {
					p.next()
					break
				}
				k, err := p.expect("name", "")
				if err != nil {
					return nil, err
				}
				if _, err := p.expect("punct", ":"); err != nil {
					return nil, err
				}
				e, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj[k.val] = e
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %s %q", t.kind, t.val)
}

// resolveValue substitutes variables and converts to plain Go values.
func resolveValue(v *gqlValue, vars map[string]any, defaults map[string]*gqlValue) any {
	switch v.kind {
	case "var":
		if val, ok := vars[v.s]; ok {
			return val
		}
		if def, ok := defaults[v.s]; ok {
			return resolveValue(def, vars, defaults)
		}
		return nil
	case "string":
		return v.s
	case "int":
		n, _ := strconv.ParseInt(v.s, 10, 64)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.s, 64)
		return f
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "enum":
		return v.s
	case "list":
		out := make([]any, len(v.list))
		for i, e := range v.list {
			out[i] = resolveValue(e, vars, defaults)
		}
		return out
	case "object":
		out := map[string]any{}
		for k, e := range v.obj {
			out[k] = resolveValue(e, vars, defaults)
		}
		return out
	}
	return nil
}

func argMap(f *gqlField, vars map[string]any, defaults map[string]*gqlValue) map[string]any {
	out := map[string]any{}
	for k, v := range f.args {
		out[k] = resolveValue(v, vars, defaults)
	}
	return out
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlErr struct{ msg string }

func (e *gqlErr) Error() string { return e.msg }

func errUnknownField(field, typ string) error {
	return &gqlErr{msg: fmt.Sprintf("Cannot query field %q on type %q.", field, typ)}
}

// gqlNode is anything a selection set can be executed against.
type gqlNode interface {
	gqlType() string
	gqlField(name string, args map[string]any) (any, error)
}

type gqlExec struct {
	vars     map[string]any
	defaults map[string]*gqlValue
	errors   []map[string]string
}

// execute runs one operation; returns (data, errors). Unknown fields abort
// the whole operation with data nil (GraphQL validation semantics).
func (e *gqlExec) execute(op *gqlOperation) (map[string]any, []map[string]string) {
	var root gqlNode
	if op.kind == "mutation" {
		root = &gqlMutationRoot{}
	} else {
		root = &gqlQueryRoot{}
	}
	data, err := e.execSelection(root, op.selection)
	if err != nil {
		return nil, append(e.errors, map[string]string{"message": err.Error()})
	}
	return data, e.errorsOrNil()
}

func (e *gqlExec) errorsOrNil() []map[string]string {
	if len(e.errors) == 0 {
		return nil
	}
	return e.errors
}

func (e *gqlExec) execSelection(node gqlNode, sel []*gqlField) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range sel {
		if f.frag != "" {
			if !typeMatches(node.gqlType(), f.frag) {
				continue
			}
			sub, err := e.execSelection(node, f.sel)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out[key] = node.gqlType()
			continue
		}
		args := argMap(f, e.vars, e.defaults)
		val, err := node.gqlField(f.name, args)
		if err != nil {
			if ue, ok := err.(*gqlErr); ok {
				return nil, ue
			}
			// runtime error: field null + recorded error
			e.errors = append(e.errors, map[string]string{"message": err.Error()})
			out[key] = nil
			continue
		}
		if f.sel == nil {
			out[key] = val
			continue
		}
		// sub-selection on a node / list / nil
		sub, err := e.execValue(val, f.sel)
		if err != nil {
			return nil, err
		}
		out[key] = sub
	}
	return out, nil
}

func typeMatches(actual, frag string) bool {
	if actual == frag {
		return true
	}
	// Node interface: Repository, Issue, PullRequest, IssueComment implement it
	if frag == "Node" {
		switch actual {
		case "Repository", "Issue", "PullRequest", "IssueComment":
			return true
		}
	}
	return false
}

func (e *gqlExec) execValue(val any, sel []*gqlField) (any, error) {
	switch v := val.(type) {
	case nil:
		return nil, nil
	case gqlNode:
		return e.execSelection(v, sel)
	case []gqlNode:
		out := make([]any, len(v))
		for i, n := range v {
			sub, err := e.execSelection(n, sel)
			if err != nil {
				return nil, err
			}
			out[i] = sub
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, el := range v {
			sub, err := e.execValue(el, sel)
			if err != nil {
				return nil, err
			}
			out[i] = sub
		}
		return out, nil
	default:
		return val, nil
	}
}

// ---------------------------------------------------------------------------
// Root types
// ---------------------------------------------------------------------------

type gqlQueryRoot struct{}

func (gqlQueryRoot) gqlType() string { return "Query" }

func (gqlQueryRoot) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "viewer":
		return &gqlUser{login: viewerLogin}, nil
	case "repository":
		owner, _ := args["owner"].(string)
		rname, _ := args["name"].(string)
		st.mu.RLock()
		repo := st.lookupRepoLocked(owner, rname)
		st.mu.RUnlock()
		if repo == nil {
			return nil, nil
		}
		return repo, nil
	case "node":
		id, _ := args["id"].(string)
		return resolveNode(id)
	case "__type":
		tname, _ := args["name"].(string)
		return typeInfo(tname), nil
	}
	return nil, errUnknownField(name, "Query")
}

type gqlMutationRoot struct{}

func (gqlMutationRoot) gqlType() string { return "Mutation" }

func (gqlMutationRoot) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "createIssue":
		input, _ := args["input"].(map[string]any)
		return mutateCreateIssue(input)
	case "closeIssue":
		input, _ := args["input"].(map[string]any)
		return mutateCloseIssue(input)
	case "createPullRequest":
		input, _ := args["input"].(map[string]any)
		return mutateCreatePR(input)
	case "mergePullRequest":
		input, _ := args["input"].(map[string]any)
		return mutateMergePR(input)
	case "addComment":
		input, _ := args["input"].(map[string]any)
		return mutateAddComment(input)
	}
	return nil, errUnknownField(name, "Mutation")
}

func resolveNode(id string) (any, error) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil, nil
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil, nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	switch parts[0] {
	case "Repository":
		rid, _ := strconv.ParseInt(parts[1], 10, 64)
		for _, r := range st.repos {
			if r.id == rid {
				return r, nil
			}
		}
	case "Issue":
		if len(parts) < 3 {
			return nil, nil
		}
		rid, _ := strconv.ParseInt(parts[1], 10, 64)
		num, _ := strconv.ParseInt(parts[2], 10, 64)
		for _, r := range st.repos {
			if r.id == rid {
				return r.issueByN[num], nil
			}
		}
	case "PullRequest":
		if len(parts) < 3 {
			return nil, nil
		}
		rid, _ := strconv.ParseInt(parts[1], 10, 64)
		num, _ := strconv.ParseInt(parts[2], 10, 64)
		for _, r := range st.repos {
			if r.id == rid {
				return r.prByN[num], nil
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

func mutateCreateIssue(input map[string]any) (any, error) {
	repoID, _ := input["repositoryId"].(string)
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	raw, err := base64.StdEncoding.DecodeString(repoID)
	if err != nil {
		return nil, fmt.Errorf("repository not found")
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 || parts[0] != "Repository" {
		return nil, fmt.Errorf("repository not found")
	}
	rid, _ := strconv.ParseInt(parts[1], 10, 64)
	st.mu.Lock()
	defer st.mu.Unlock()
	var repo *repo
	for _, r := range st.repos {
		if r.id == rid {
			repo = r
			break
		}
	}
	if repo == nil {
		return nil, fmt.Errorf("repository not found")
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
	return &gqlPayload{typ: "CreateIssuePayload", fields: map[string]any{"issue": is}}, nil
}

func mutateCloseIssue(input map[string]any) (any, error) {
	issueID, _ := input["issueId"].(string)
	raw, err := base64.StdEncoding.DecodeString(issueID)
	if err != nil {
		return nil, fmt.Errorf("issue not found")
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != "Issue" {
		return nil, fmt.Errorf("issue not found")
	}
	rid, _ := strconv.ParseInt(parts[1], 10, 64)
	num, _ := strconv.ParseInt(parts[2], 10, 64)
	st.mu.Lock()
	defer st.mu.Unlock()
	var is *issue
	var repo *repo
	for _, r := range st.repos {
		if r.id == rid {
			repo = r
			is = r.issueByN[num]
			break
		}
	}
	if is == nil {
		return nil, fmt.Errorf("issue not found")
	}
	is.state = "closed"
	is.refreshCache(repo)
	return &gqlPayload{typ: "CloseIssuePayload", fields: map[string]any{"issue": is}}, nil
}

func mutateCreatePR(input map[string]any) (any, error) {
	repoID, _ := input["repositoryId"].(string)
	baseRef, _ := input["baseRefName"].(string)
	headRef, _ := input["headRefName"].(string)
	title, _ := input["title"].(string)
	raw, err := base64.StdEncoding.DecodeString(repoID)
	if err != nil {
		return nil, fmt.Errorf("repository not found")
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 || parts[0] != "Repository" {
		return nil, fmt.Errorf("repository not found")
	}
	rid, _ := strconv.ParseInt(parts[1], 10, 64)
	st.mu.Lock()
	defer st.mu.Unlock()
	var repo *repo
	for _, r := range st.repos {
		if r.id == rid {
			repo = r
			break
		}
	}
	if repo == nil {
		return nil, fmt.Errorf("repository not found")
	}
	headSHA, okH := repo.resolveRef(headRef)
	baseSHA, okB := repo.resolveRef(baseRef)
	if !okH || !okB {
		return nil, fmt.Errorf("head or base ref does not exist")
	}
	st.prSeq++
	pr := &pullRequest{
		id:        st.prSeq,
		number:    int64(len(repo.prs)) + 1,
		title:     title,
		state:     "open",
		headRef:   headRef,
		headSHA:   headSHA,
		baseRef:   baseRef,
		baseSHA:   baseSHA,
		createdAt: stamp(st.nextClock()),
	}
	repo.prs = append(repo.prs, pr)
	repo.prByN[pr.number] = pr
	return &gqlPayload{typ: "CreatePullRequestPayload", fields: map[string]any{"pullRequest": pr}}, nil
}

func mutateMergePR(input map[string]any) (any, error) {
	prID, _ := input["pullRequestId"].(string)
	raw, err := base64.StdEncoding.DecodeString(prID)
	if err != nil {
		return nil, fmt.Errorf("pull request not found")
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != "PullRequest" {
		return nil, fmt.Errorf("pull request not found")
	}
	rid, _ := strconv.ParseInt(parts[1], 10, 64)
	num, _ := strconv.ParseInt(parts[2], 10, 64)
	st.mu.Lock()
	defer st.mu.Unlock()
	var pr *pullRequest
	var repo *repo
	for _, r := range st.repos {
		if r.id == rid {
			repo = r
			pr = r.prByN[num]
			break
		}
	}
	if pr == nil {
		return nil, fmt.Errorf("pull request not found")
	}
	if pr.merged {
		return nil, fmt.Errorf("pull request already merged")
	}
	baseTip, ok := repo.refs["refs/heads/"+pr.baseRef]
	if !ok {
		return nil, fmt.Errorf("base ref not found")
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
	return &gqlPayload{typ: "MergePullRequestPayload", fields: map[string]any{"pullRequest": pr}}, nil
}

func mutateAddComment(input map[string]any) (any, error) {
	subjectID, _ := input["subjectId"].(string)
	body, _ := input["body"].(string)
	raw, err := base64.StdEncoding.DecodeString(subjectID)
	if err != nil {
		return nil, fmt.Errorf("subject not found")
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != "Issue" {
		return nil, fmt.Errorf("subject not found")
	}
	rid, _ := strconv.ParseInt(parts[1], 10, 64)
	num, _ := strconv.ParseInt(parts[2], 10, 64)
	st.mu.Lock()
	defer st.mu.Unlock()
	var is *issue
	var repo *repo
	for _, r := range st.repos {
		if r.id == rid {
			repo = r
			is = r.issueByN[num]
			break
		}
	}
	if is == nil {
		return nil, fmt.Errorf("subject not found")
	}
	st.cmtSeq++
	c := &comment{id: st.cmtSeq, body: body, createdAt: stamp(st.nextClock())}
	repo.comments[num] = append(repo.comments[num], c)
	edge := &gqlCommentEdge{node: c}
	return &gqlPayload{typ: "AddCommentPayload", fields: map[string]any{"commentEdge": edge}}, nil
}

// ---------------------------------------------------------------------------
// Node implementations
// ---------------------------------------------------------------------------

type gqlPayload struct {
	typ    string
	fields map[string]any
}

func (p *gqlPayload) gqlType() string { return p.typ }
func (p *gqlPayload) gqlField(name string, args map[string]any) (any, error) {
	if v, ok := p.fields[name]; ok {
		return v, nil
	}
	if name == "clientMutationId" {
		return nil, nil
	}
	return nil, errUnknownField(name, p.typ)
}

type gqlUser struct{ login string }

func (gqlUser) gqlType() string { return "User" }
func (u *gqlUser) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "login":
		return u.login, nil
	case "id":
		return viewerID, nil
	case "name":
		return viewerName, nil
	case "email":
		return viewerEmail, nil
	}
	return nil, errUnknownField(name, "User")
}

// repo implements gqlNode.
func (r *repo) gqlType() string { return "Repository" }
func (r *repo) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return repoGlobalID(r), nil
	case "databaseId":
		return r.id, nil
	case "name":
		return r.name, nil
	case "nameWithOwner":
		return viewerLogin + "/" + r.name, nil
	case "owner":
		return &gqlUser{login: viewerLogin}, nil
	case "defaultBranchRef":
		if _, ok := r.refs["refs/heads/main"]; !ok {
			return nil, nil
		}
		return &gqlRef{name: "main"}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		num := intArg(args, "number", 0)
		if is := r.issueByN[int64(num)]; is != nil {
			return is, nil
		}
		return nil, nil
	case "issues":
		return buildIssueConn(r, args), nil
	case "pullRequests":
		return buildPRConn(r, args), nil
	case "pullRequest":
		num := intArg(args, "number", 0)
		if pr := r.prByN[int64(num)]; pr != nil {
			return pr, nil
		}
		return nil, nil
	}
	return nil, errUnknownField(name, "Repository")
}

type gqlRef struct{ name string }

func (gqlRef) gqlType() string { return "Ref" }
func (rf *gqlRef) gqlField(name string, args map[string]any) (any, error) {
	if name == "name" {
		return rf.name, nil
	}
	return nil, errUnknownField(name, "Ref")
}

// issue implements gqlNode.
func (is *issue) gqlType() string { return "Issue" }
func (is *issue) gqlField(name string, args map[string]any) (any, error) {
	// need the repo for global ids; find it by scanning (cheap enough)
	switch name {
	case "id":
		return issueGlobalID(findIssueRepo(is), is), nil
	case "number":
		return is.number, nil
	case "title":
		return is.title, nil
	case "body":
		return is.body, nil
	case "state":
		if is.state == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "url":
		r := findIssueRepo(is)
		if r == nil {
			return "", nil
		}
		return "https://example.invalid/" + viewerLogin + "/" + r.name + "/issues/" + strconv.FormatInt(is.number, 10), nil
	case "createdAt":
		return is.createdAt, nil
	case "author":
		return &gqlUser{login: viewerLogin}, nil
	}
	return nil, errUnknownField(name, "Issue")
}

func findIssueRepo(is *issue) *repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, r := range st.repos {
		if r.issueByN[is.number] == is {
			return r
		}
	}
	return nil
}

// pullRequest implements gqlNode.
func (pr *pullRequest) gqlType() string { return "PullRequest" }
func (pr *pullRequest) gqlField(name string, args map[string]any) (any, error) {
	r := findPRRepo(pr)
	switch name {
	case "id":
		return prGlobalID(r, pr), nil
	case "number":
		return pr.number, nil
	case "title":
		return pr.title, nil
	case "body":
		return pr.body, nil
	case "state":
		if pr.merged {
			return "CLOSED", nil
		}
		if pr.state == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "merged":
		return pr.merged, nil
	case "mergeable":
		if pr.merged {
			return "UNKNOWN", nil
		}
		if r != nil && prConflict(r, pr) {
			return "CONFLICTING", nil
		}
		return "MERGEABLE", nil
	case "headRefName":
		return pr.headRef, nil
	case "baseRefName":
		return pr.baseRef, nil
	case "files":
		first := intArg(args, "first", -1)
		var nodes []gqlNode
		if r != nil {
			for _, f := range prFiles(r, pr) {
				nodes = append(nodes, &gqlFile{path: f.Filename})
			}
		}
		if first >= 0 && len(nodes) > first {
			nodes = nodes[:first]
		}
		return &gqlFileConn{nodes: nodes}, nil
	case "url":
		if r == nil {
			return "", nil
		}
		return "https://example.invalid/" + viewerLogin + "/" + r.name + "/pull/" + strconv.FormatInt(pr.number, 10), nil
	case "createdAt":
		return pr.createdAt, nil
	}
	return nil, errUnknownField(name, "PullRequest")
}

func findPRRepo(pr *pullRequest) *repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, r := range st.repos {
		if r.prByN[pr.number] == pr {
			return r
		}
	}
	return nil
}

func prConflict(r *repo, pr *pullRequest) bool {
	baseTip, ok := r.refs["refs/heads/"+pr.baseRef]
	if !ok {
		return false
	}
	mb := r.mergeBase(baseTip, pr.headSHA)
	if mb == "" {
		return false
	}
	mbFlat := map[string]treeEntry{}
	headFlat := map[string]treeEntry{}
	baseFlat := map[string]treeEntry{}
	r.flattenTree(r.commitTree(mb), "", mbFlat)
	r.flattenTree(r.commitTree(pr.headSHA), "", headFlat)
	r.flattenTree(r.commitTree(baseTip), "", baseFlat)
	for path, h := range headFlat {
		m, inMB := mbFlat[path]
		b, inBase := baseFlat[path]
		if inMB && h.oid != m.oid && inBase && b.oid != m.oid && h.oid != b.oid {
			return true
		}
	}
	for path, b := range baseFlat {
		m, inMB := mbFlat[path]
		if _, inHead := headFlat[path]; !inHead && inMB && b.oid != m.oid {
			return true
		}
	}
	return false
}

type gqlFile struct{ path string }

func (gqlFile) gqlType() string { return "PullRequestFile" }
func (f *gqlFile) gqlField(name string, args map[string]any) (any, error) {
	if name == "path" {
		return f.path, nil
	}
	return nil, errUnknownField(name, "PullRequestFile")
}

type gqlFileConn struct{ nodes []gqlNode }

func (gqlFileConn) gqlType() string { return "PullRequestFileConnection" }
func (c *gqlFileConn) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "nodes":
		return c.nodes, nil
	case "pageInfo":
		return &gqlPageInfo{hasNext: false, endCursor: ""}, nil
	case "totalCount":
		return int64(len(c.nodes)), nil
	}
	return nil, errUnknownField(name, "PullRequestFileConnection")
}

type gqlPageInfo struct {
	hasNext   bool
	endCursor string
	hasPrev   bool
	startCursor string
}

func (gqlPageInfo) gqlType() string { return "PageInfo" }
func (p *gqlPageInfo) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "hasNextPage":
		return p.hasNext, nil
	case "hasPreviousPage":
		return p.hasPrev, nil
	case "endCursor":
		if p.endCursor == "" {
			return nil, nil
		}
		return p.endCursor, nil
	case "startCursor":
		if p.startCursor == "" {
			return nil, nil
		}
		return p.startCursor, nil
	}
	return nil, errUnknownField(name, "PageInfo")
}

type gqlIssueConn struct {
	nodes     []gqlNode
	hasNext   bool
	endCursor string
	total     int
}

func (gqlIssueConn) gqlType() string { return "IssueConnection" }
func (c *gqlIssueConn) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "nodes":
		return c.nodes, nil
	case "pageInfo":
		return &gqlPageInfo{hasNext: c.hasNext, endCursor: c.endCursor}, nil
	case "totalCount":
		return int64(c.total), nil
	}
	return nil, errUnknownField(name, "IssueConnection")
}

type gqlPRConn struct {
	nodes   []gqlNode
	hasNext bool
	total   int
}

func (gqlPRConn) gqlType() string { return "PullRequestConnection" }
func (c *gqlPRConn) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "nodes":
		return c.nodes, nil
	case "pageInfo":
		return &gqlPageInfo{hasNext: c.hasNext, endCursor: ""}, nil
	case "totalCount":
		return int64(c.total), nil
	}
	return nil, errUnknownField(name, "PullRequestConnection")
}

type gqlCommentEdge struct{ node *comment }

func (gqlCommentEdge) gqlType() string { return "IssueCommentEdge" }
func (e *gqlCommentEdge) gqlField(name string, args map[string]any) (any, error) {
	if name == "node" {
		return e.node, nil
	}
	return nil, errUnknownField(name, "IssueCommentEdge")
}

// comment implements gqlNode.
func (c *comment) gqlType() string { return "IssueComment" }
func (c *comment) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return globalID("IssueComment", strconv.FormatInt(c.id, 10)), nil
	case "body":
		return c.body, nil
	case "author":
		return &gqlUser{login: viewerLogin}, nil
	case "createdAt":
		return c.createdAt, nil
	}
	return nil, errUnknownField(name, "IssueComment")
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

func cursorFor(n int64) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(n, 10)))
}

func cursorNum(s string) (int64, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 || parts[0] != "cursor" {
		return 0, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func buildIssueConn(r *repo, args map[string]any) *gqlIssueConn {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var states []string
	if v, ok := args["states"]; ok {
		if arr, ok := v.([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok {
					states = append(states, s)
				}
			}
		}
	}
	dir := "DESC"
	field := "CREATED_AT"
	if v, ok := args["orderBy"]; ok {
		if m, ok := v.(map[string]any); ok {
			if d, ok := m["direction"].(string); ok {
				dir = d
			}
			if f, ok := m["field"].(string); ok {
				field = f
			}
		}
	}
	_ = field
	var list []*issue
	for _, is := range r.issues {
		st := "OPEN"
		if is.state == "closed" {
			st = "CLOSED"
		}
		if len(states) > 0 {
			found := false
			for _, s := range states {
				if s == st {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		list = append(list, is)
	}
	if dir == "ASC" {
		sort.Slice(list, func(i, j int) bool { return list[i].number < list[j].number })
	} else {
		sort.Slice(list, func(i, j int) bool { return list[i].number > list[j].number })
	}
	// cursor filter
	if v, ok := args["after"]; ok {
		if s, ok := v.(string); ok && s != "" {
			if n, ok := cursorNum(s); ok {
				var filtered []*issue
				for _, is := range list {
					if dir == "ASC" && is.number > n {
						filtered = append(filtered, is)
					} else if dir == "DESC" && is.number < n {
						filtered = append(filtered, is)
					}
				}
				list = filtered
			}
		}
	}
	total := len(list)
	first := intArg(args, "first", -1)
	hasNext := false
	if first >= 0 && len(list) > first {
		hasNext = true
		list = list[:first]
	}
	endCursor := ""
	if len(list) > 0 {
		endCursor = cursorFor(list[len(list)-1].number)
	}
	nodes := make([]gqlNode, len(list))
	for i, is := range list {
		nodes[i] = is
	}
	return &gqlIssueConn{nodes: nodes, hasNext: hasNext, endCursor: endCursor, total: total}
}

func buildPRConn(r *repo, args map[string]any) *gqlPRConn {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var list []*pullRequest
	for i := len(r.prs) - 1; i >= 0; i-- {
		list = append(list, r.prs[i])
	}
	total := len(list)
	first := intArg(args, "first", -1)
	hasNext := false
	if first >= 0 && len(list) > first {
		hasNext = true
		list = list[:first]
	}
	nodes := make([]gqlNode, len(list))
	for i, pr := range list {
		nodes[i] = pr
	}
	return &gqlPRConn{nodes: nodes, hasNext: hasNext, total: total}
}

func intArg(args map[string]any, name string, def int) int {
	if v, ok := args[name]; ok {
		switch n := v.(type) {
		case int64:
			return int(n)
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Introspection (__type)
// ---------------------------------------------------------------------------

type gqlTypeInfo struct {
	name       string
	fields     []string
	enumValues []string
}

func (gqlTypeInfo) gqlType() string { return "__Type" }
func (t *gqlTypeInfo) gqlField(name string, args map[string]any) (any, error) {
	switch name {
	case "name":
		return t.name, nil
	case "kind":
		if len(t.enumValues) > 0 {
			return "ENUM", nil
		}
		return "OBJECT", nil
	case "fields":
		var nodes []gqlNode
		for _, f := range t.fields {
			nodes = append(nodes, &gqlFieldInfo{name: f})
		}
		return nodes, nil
	case "enumValues":
		var nodes []gqlNode
		for _, e := range t.enumValues {
			nodes = append(nodes, &gqlEnumValue{name: e})
		}
		return nodes, nil
	}
	return nil, errUnknownField(name, "__Type")
}

type gqlFieldInfo struct{ name string }

func (gqlFieldInfo) gqlType() string { return "__Field" }
func (f *gqlFieldInfo) gqlField(name string, args map[string]any) (any, error) {
	if name == "name" {
		return f.name, nil
	}
	return nil, errUnknownField(name, "__Field")
}

type gqlEnumValue struct{ name string }

func (gqlEnumValue) gqlType() string { return "__EnumValue" }
func (e *gqlEnumValue) gqlField(name string, args map[string]any) (any, error) {
	if name == "name" {
		return e.name, nil
	}
	return nil, errUnknownField(name, "__EnumValue")
}

func typeInfo(name string) *gqlTypeInfo {
	switch name {
	case "Issue":
		return &gqlTypeInfo{name: "Issue", fields: []string{"id", "number", "title", "state", "body", "url", "createdAt", "author"}}
	case "PullRequest":
		return &gqlTypeInfo{name: "PullRequest", fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body", "url", "createdAt"}}
	case "Repository":
		return &gqlTypeInfo{name: "Repository", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}}
	case "SearchType":
		return &gqlTypeInfo{name: "SearchType", enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}}
	case "User":
		return &gqlTypeInfo{name: "User", fields: []string{"login", "id", "name", "email"}}
	case "IssueConnection":
		return &gqlTypeInfo{name: "IssueConnection", fields: []string{"nodes", "pageInfo", "totalCount"}}
	case "PullRequestConnection":
		return &gqlTypeInfo{name: "PullRequestConnection", fields: []string{"nodes", "pageInfo", "totalCount"}}
	case "PageInfo":
		return &gqlTypeInfo{name: "PageInfo", fields: []string{"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"}}
	case "Ref":
		return &gqlTypeInfo{name: "Ref", fields: []string{"name"}}
	case "IssueComment":
		return &gqlTypeInfo{name: "IssueComment", fields: []string{"id", "body", "author", "createdAt"}}
	case "Query":
		return &gqlTypeInfo{name: "Query", fields: []string{"viewer", "repository", "node", "__type"}}
	case "Mutation":
		return &gqlTypeInfo{name: "Mutation", fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]any{"errors": []map[string]string{{"message": "bad request"}}})
		return
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []map[string]string{{"message": "bad request"}}})
		return
	}
	// batch?
	var batch []map[string]any
	isBatch := false
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSON(w, 400, map[string]any{"errors": []map[string]string{{"message": "bad request"}}})
			return
		}
		isBatch = true
	}
	if isBatch {
		var out []any
		for _, req := range batch {
			out = append(out, execGraphQLRequest(req))
		}
		writeJSON(w, 200, out)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []map[string]string{{"message": "bad request"}}})
		return
	}
	writeJSON(w, 200, execGraphQLRequest(req))
}

func execGraphQLRequest(req map[string]any) any {
	query, _ := req["query"].(string)
	if query == "" {
		return map[string]any{"errors": []map[string]string{{"message": "query is required"}}}
	}
	op, err := parseGraphQL(query)
	if err != nil {
		return map[string]any{"errors": []map[string]string{{"message": err.Error()}}}
	}
	vars := map[string]any{}
	if v, ok := req["variables"]; ok {
		if m, ok := v.(map[string]any); ok {
			vars = m
		}
	}
	ex := &gqlExec{vars: vars, defaults: op.varDefs}
	data, errs := ex.execute(op)
	resp := map[string]any{}
	if data == nil {
		resp["data"] = nil
	} else {
		resp["data"] = data
	}
	if errs != nil {
		resp["errors"] = errs
	}
	return resp
}
