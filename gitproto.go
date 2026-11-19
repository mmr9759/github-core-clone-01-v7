package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Git smart-HTTP endpoints
// ---------------------------------------------------------------------------

const (
	uploadPackCapsV0  = "side-band-64k multi_ack_detailed thin-pack ofs-delta shallow agent=gitforge/1.0 object-format=sha1 symref=HEAD:refs/heads/main"
	receivePackCapsV0 = "report-status report-status-v2 delete-refs side-band-64k ofs-delta quiet atomic agent=gitforge/1.0"
)

// gitRepoFromPath extracts (owner, repo) from /{owner}/{repo}.git/...
func gitRepoFromPath(p string) (owner, repo string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 2 || !strings.HasSuffix(parts[1], ".git") {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// handleInfoRefs serves GET /{owner}/{repo}.git/info/refs?service=...
func handleInfoRefs(w http.ResponseWriter, r *http.Request) {
	owner, name, ok := gitRepoFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	service := r.URL.Query().Get("service")
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	st.mu.RUnlock()
	if repo == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	switch service {
	case "git-upload-pack":
		if strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
			writeGitAdvert(w, "application/x-git-upload-pack-advertisement", v2UploadAdvert())
			return
		}
		key := "up:" + repo.name + ":" + fmt.Sprint(repo.version)
		if cached, hit := st.advCache.Load(key); hit {
			writeGitAdvert(w, "application/x-git-upload-pack-advertisement", cached.([]byte))
			return
		}
		st.mu.RLock()
		body := buildV0Advert(repo, uploadPackCapsV0)
		st.mu.RUnlock()
		full := appendServicePreamble("git-upload-pack", body)
		st.advCache.Store(key, full)
		writeGitAdvert(w, "application/x-git-upload-pack-advertisement", full)
	case "git-receive-pack":
		key := "rp:" + repo.name + ":" + fmt.Sprint(repo.version)
		if cached, hit := st.advCache.Load(key); hit {
			writeGitAdvert(w, "application/x-git-receive-pack-advertisement", cached.([]byte))
			return
		}
		st.mu.RLock()
		body := buildV0Advert(repo, receivePackCapsV0)
		st.mu.RUnlock()
		full := appendServicePreamble("git-receive-pack", body)
		st.advCache.Store(key, full)
		writeGitAdvert(w, "application/x-git-receive-pack-advertisement", full)
	default:
		writeJSON(w, 400, errJSON("unsupported service"))
	}
}

// appendServicePreamble prepends the "# service=..." line + flush that every
// smart-HTTP info/refs response carries.
func appendServicePreamble(service string, advert []byte) []byte {
	var out bytes.Buffer
	out.Write(pktString("# service=" + service + "\n"))
	out.WriteString(pktFlush)
	out.Write(advert)
	return out.Bytes()
}

// v2UploadAdvert is the static protocol-v2 capability advertisement.
func v2UploadAdvert() []byte {
	var out bytes.Buffer
	out.Write(pktString("# service=git-upload-pack\n"))
	out.WriteString(pktFlush)
	out.Write(pktString("version 2\n"))
	out.Write(pktString("agent=gitforge/1.0\n"))
	out.Write(pktString("ls-refs=unborn\n"))
	out.Write(pktString("fetch=shallow wait-for-done\n"))
	out.Write(pktString("object-format=sha1\n"))
	out.WriteString(pktFlush)
	return out.Bytes()
}

func writeGitAdvert(w http.ResponseWriter, ctype string, body []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(200)
	w.Write(body)
}

// buildV0Advert builds a v0 advertisement: service line, ref lines, flush.
// The first ref line carries the capabilities after a NUL byte.
func buildV0Advert(repo *repo, caps string) []byte {
	var out bytes.Buffer
	// refs sorted; HEAD first, then every ref including refs/heads/main
	tip, hasTip := repo.defaultTip()
	lines := []string{}
	if hasTip {
		lines = append(lines, tip+" HEAD")
	}
	for _, name := range repo.sortedRefNames() {
		lines = append(lines, repo.refs[name]+" "+name)
	}
	// peeled lines for annotated tags, appended after their tag line
	var peeled []string
	for _, name := range repo.sortedRefNames() {
		if !strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		oid := repo.refs[name]
		if obj, ok := repo.objects[oid]; ok && obj.typ == objTag {
			target := tagTarget(obj.data)
			if target != "" {
				peeled = append(peeled, target+" "+name+"^{}")
			}
		}
	}
	if len(lines) == 0 {
		// empty repo: capabilities^{} line
		out.Write(pktString(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		out.WriteString(pktFlush)
		return out.Bytes()
	}
	for i, ln := range lines {
		if i == 0 {
			out.Write(pktString(ln + "\x00" + caps + "\n"))
		} else {
			out.Write(pktString(ln + "\n"))
		}
	}
	for _, p := range peeled {
		out.Write(pktString(p + "\n"))
	}
	out.WriteString(pktFlush)
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func handleUploadPack(w http.ResponseWriter, r *http.Request) {
	owner, name, ok := gitRepoFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	st.mu.RUnlock()
	if repo == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		writeJSON(w, 400, errJSON("empty request"))
		return
	}
	proto := r.Header.Get("Git-Protocol")
	if strings.Contains(proto, "version=2") {
		handleUploadPackV2(w, repo, body)
		return
	}
	handleUploadPackV0(w, repo, body)
}

// parseV2Args splits a v2 request body into the pre-delim and post-delim
// argument line lists.
func parseV2Args(body []byte) (pre, post [][]byte, err error) {
	r := &pktReader{b: body}
	seenDelim := false
	for {
		payload, ctrl, err := r.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch ctrl {
		case "delim":
			seenDelim = true
		case "flush":
			return pre, post, nil
		default:
			if seenDelim {
				post = append(post, payload)
			} else {
				pre = append(pre, payload)
			}
		}
	}
	return pre, post, nil
}

func handleUploadPackV2(w http.ResponseWriter, repo *repo, body []byte) {
	pre, post, err := parseV2Args(body)
	if err != nil || len(pre) == 0 {
		writeJSON(w, 400, errJSON("bad v2 request"))
		return
	}
	cmd := strings.TrimSuffix(string(pre[0]), "\n")
	switch cmd {
	case "command=ls-refs":
		handleLsRefs(w, repo, pre, post)
	case "command=fetch":
		handleFetchV2(w, repo, pre, post)
	default:
		writeJSON(w, 400, errJSON("unsupported command"))
	}
}

func handleLsRefs(w http.ResponseWriter, repo *repo, pre, post [][]byte) {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, line := range append(pre[1:], post...) {
		s := strings.TrimSuffix(string(line), "\n")
		switch {
		case s == "peel":
			peel = true
		case s == "symrefs":
			symrefs = true
		case s == "unborn":
			unborn = true
		case strings.HasPrefix(s, "ref-prefix "):
			prefixes = append(prefixes, s[len("ref-prefix "):])
		}
	}
	// ls-refs is deterministic per (repo, version, args): cache the bytes.
	lsKey := fmt.Sprintf("ls:%s:%d:%v:%v:%v:%v", repo.name, repo.version, peel, symrefs, unborn, prefixes)
	if cached, hit := st.advCache.Load(lsKey); hit {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(200)
		w.Write(cached.([]byte))
		return
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out bytes.Buffer
	tip, hasTip := repo.defaultTip()
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	// HEAD line
	if match("HEAD") {
		if hasTip {
			line := tip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			out.Write(pktString(line + "\n"))
		} else if unborn {
			out.Write(pktString("unborn HEAD symref-target:refs/heads/main\n"))
		}
	}
	for _, name := range repo.sortedRefNames() {
		if !match(name) {
			continue
		}
		oid := repo.refs[name]
		line := oid + " " + name
		if peel {
			if obj, ok := repo.objects[oid]; ok && obj.typ == objTag {
				if target := tagTarget(obj.data); target != "" {
					line += " peeled:" + target
				}
			}
		}
		out.Write(pktString(line + "\n"))
	}
	out.WriteString(pktFlush)
	body := out.Bytes()
	st.advCache.Store(lsKey, body)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(200)
	w.Write(body)
}

func handleFetchV2(w http.ResponseWriter, repo *repo, pre, post [][]byte) {
	var wants, haves, shallowList []string
	depth := 0
	for _, line := range append(pre[1:], post...) {
		s := strings.TrimSuffix(string(line), "\n")
		switch {
		case strings.HasPrefix(s, "want "):
			wants = append(wants, s[5:])
		case strings.HasPrefix(s, "have "):
			haves = append(haves, s[5:])
		case strings.HasPrefix(s, "shallow "):
			shallowList = append(shallowList, s[8:])
		case strings.HasPrefix(s, "deepen "):
			fmt.Sscanf(s[7:], "%d", &depth)
		}
	}
	if len(wants) == 0 {
		writeJSON(w, 400, errJSON("no wants"))
		return
	}
	// cache key: content-addressed by wants/haves/depth/shallow + repo tip
	key := fetchCacheKey(repo, wants, haves, shallowList, depth)
	if cached, hit := st.packCache.Load(key); hit {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(200)
		w.Write(cached.([]byte))
		return
	}
	// singleflight: one builder per key
	done := make(chan struct{})
	actual, loaded := st.packBuild.LoadOrStore(key, done)
	if loaded {
		<-actual.(chan struct{})
		if cached, hit := st.packCache.Load(key); hit {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Header().Set("X-GitHub-Request-Id", nextReqID())
			w.WriteHeader(200)
			w.Write(cached.([]byte))
			return
		}
		// fall through and build (cache was evicted)
		done = make(chan struct{})
		st.packBuild.Store(key, done)
		defer close(done)
	} else {
		defer close(done)
	}
	st.mu.RLock()
	objs, shallowOut, unshallowOut := repo.fetchClosure(wants, haves, shallowList, depth)
	acks := commonAcks(repo, wants, haves)
	st.mu.RUnlock()
	pack := writePack(objs)
	var out bytes.Buffer
	out.Write(pktString("acknowledgments\n"))
	if len(acks) > 0 {
		for _, a := range acks {
			out.Write(pktString("ACK " + a + "\n"))
		}
		out.Write(pktString("ready\n"))
	} else {
		out.Write(pktString("NAK\n"))
	}
	out.WriteString(pktDelim)
	if len(shallowOut) > 0 || len(unshallowOut) > 0 {
		out.Write(pktString("shallow-info\n"))
		for _, s := range shallowOut {
			out.Write(pktString("shallow " + s + "\n"))
		}
		for _, s := range unshallowOut {
			out.Write(pktString("unshallow " + s + "\n"))
		}
		out.WriteString(pktDelim)
	}
	out.Write(pktString("packfile\n"))
	out.Write(sidebandWrap(pack))
	body := out.Bytes()
	st.packCache.Store(key, body)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(200)
	w.Write(body)
}

// commonAcks returns the haves that are ancestors-or-equal of a want.
func commonAcks(repo *repo, wants, haves []string) []string {
	var out []string
	for _, h := range haves {
		for _, w := range wants {
			if repo.isAncestor(h, w) {
				out = append(out, h)
				break
			}
		}
	}
	return dedupStr(out)
}

func fetchCacheKey(repo *repo, wants, haves, shallow []string, depth int) string {
	w := append([]string{}, wants...)
	h := append([]string{}, haves...)
	s := append([]string{}, shallow...)
	sort.Strings(w)
	sort.Strings(h)
	sort.Strings(s)
	return fmt.Sprintf("f:%s:%d:%v:%v:%v:%d", repo.name, repo.version, w, h, s, depth)
}

func handleUploadPackV0(w http.ResponseWriter, repo *repo, body []byte) {
	// v0 fetch: want lines (+features on first), flush, have lines, done, flush
	lines, err := readPktLines(body)
	if err != nil {
		writeJSON(w, 400, errJSON("bad v0 request"))
		return
	}
	var wants, haves []string
	depth := 0
	for _, ln := range lines {
		s := string(ln)
		if strings.HasPrefix(s, "\x00") {
			continue // control packet marker
		}
		s = strings.TrimSuffix(s, "\n")
		// v0 fetch: the first want line carries capabilities after a space
		fields := strings.Fields(s)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "want":
			if len(fields) >= 2 {
				wants = append(wants, fields[1])
			}
		case "have":
			if len(fields) >= 2 {
				haves = append(haves, fields[1])
			}
		case "deepen":
			if len(fields) >= 2 {
				fmt.Sscanf(fields[1], "%d", &depth)
			}
		}
	}
	if len(wants) == 0 {
		writeJSON(w, 400, errJSON("no wants"))
		return
	}
	st.mu.RLock()
	objs, shallowOut, _ := repo.fetchClosure(wants, haves, nil, depth)
	acks := commonAcks(repo, wants, haves)
	st.mu.RUnlock()
	pack := writePack(objs)
	var out bytes.Buffer
	if len(acks) > 0 {
		for _, a := range acks {
			out.Write(pktString("ACK " + a + "\n"))
		}
	} else {
		out.Write(pktString("NAK\n"))
	}
	for _, s := range shallowOut {
		out.Write(pktString("shallow " + s + "\n"))
	}
	out.Write(sidebandWrap(pack))
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(200)
	w.Write(out.Bytes())
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

func handleReceivePack(w http.ResponseWriter, r *http.Request) {
	owner, name, ok := gitRepoFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	st.mu.RLock()
	repo := st.lookupRepoLocked(owner, name)
	st.mu.RUnlock()
	if repo == nil {
		writeJSON(w, 404, errJSON("Not Found"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, errJSON("bad request"))
		return
	}
	// split: command pkt-lines until flush, then the pack bytes
	rdr := &pktReader{b: body}
	type cmd struct {
		old, new, ref string
	}
	var cmds []cmd
	packStart := 0
	for {
		payload, ctrl, err := rdr.next()
		if err != nil {
			writeJSON(w, 400, errJSON("bad receive-pack request"))
			return
		}
		if ctrl == "flush" {
			packStart = rdr.pos
			break
		}
		if ctrl != "" {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			writeJSON(w, 400, errJSON("bad command line"))
			return
		}
		cmds = append(cmds, cmd{old: parts[0], new: parts[1], ref: parts[2]})
	}
	pack := body[packStart:]
	var objs []*gitObject
	if len(pack) > 0 {
		objs, err = readPack(pack)
		if err != nil {
			// malformed pack: still answer with an unpack failure report
			report := pktString("unpack error\n")
			report = append(report, []byte(pktFlush)...)
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			w.Header().Set("X-GitHub-Request-Id", nextReqID())
			w.WriteHeader(200)
			w.Write(sidebandWrap(report))
			return
		}
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	// store objects (dedup by oid)
	for _, o := range objs {
		oid := gitOID(o.typ, o.data)
		if _, ok := repo.objects[oid]; !ok {
			repo.objects[oid] = o
		}
	}
	var report bytes.Buffer
	report.Write(pktString("unpack ok\n"))
	changed := false
	for _, c := range cmds {
		cur, exists := repo.refs[c.ref]
		switch {
		case c.new == zeroOID: // delete
			if !exists {
				report.Write(pktString("ng " + c.ref + " remote ref does not exist\n"))
			} else if c.old != zeroOID && c.old != cur {
				report.Write(pktString("ng " + c.ref + " old value does not match\n"))
			} else {
				delete(repo.refs, c.ref)
				report.Write(pktString("ok " + c.ref + "\n"))
				changed = true
			}
		case c.old == zeroOID: // create
			if exists {
				report.Write(pktString("ng " + c.ref + " already exists\n"))
			} else if _, ok := repo.objects[c.new]; !ok {
				report.Write(pktString("ng " + c.ref + " object not found\n"))
			} else {
				repo.refs[c.ref] = c.new
				report.Write(pktString("ok " + c.ref + "\n"))
				changed = true
			}
		default: // update: fast-forward check
			if !exists {
				report.Write(pktString("ng " + c.ref + " remote ref does not exist\n"))
			} else if c.old != cur {
				report.Write(pktString("ng " + c.ref + " old value does not match\n"))
			} else if c.new == c.old {
				report.Write(pktString("ok " + c.ref + "\n"))
			} else if _, ok := repo.objects[c.new]; !ok {
				report.Write(pktString("ng " + c.ref + " object not found\n"))
			} else if !repo.isAncestor(c.old, c.new) {
				report.Write(pktString("ng " + c.ref + " non-fast-forward\n"))
			} else {
				repo.refs[c.ref] = c.new
				report.Write(pktString("ok " + c.ref + "\n"))
				changed = true
			}
		}
	}
	if changed {
		repo.version++
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(200)
	w.Write(sidebandWrap(report.Bytes()))
}
