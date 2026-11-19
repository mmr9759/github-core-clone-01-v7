package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line framing (git wire protocol)
// ---------------------------------------------------------------------------

const (
	pktFlush = "0000"
	pktDelim = "0001"
	pktRespEnd = "0002"
)

// pktLine encodes one line (payload must include its trailing \n when wanted).
func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, payload...)
	return out
}

func pktString(s string) []byte { return pktLine([]byte(s)) }

// pktReader decodes a pkt-line stream from a byte slice.
type pktReader struct {
	b   []byte
	pos int
}

// next returns the next packet payload, or (nil, "flush"|"delim"|"end", nil)
// for control packets, or ("", "", io.EOF) at end of input.
func (p *pktReader) next() ([]byte, string, error) {
	if p.pos+4 > len(p.b) {
		return nil, "", io.EOF
	}
	hexLen := string(p.b[p.pos : p.pos+4])
	n, err := strconv.ParseInt(hexLen, 16, 32)
	if err != nil {
		return nil, "", fmt.Errorf("bad pkt-line length %q", hexLen)
	}
	p.pos += 4
	switch n {
	case 0:
		return nil, "flush", nil
	case 1:
		return nil, "delim", nil
	case 2:
		return nil, "end", nil
	}
	if p.pos+int(n)-4 > len(p.b) {
		return nil, "", io.ErrUnexpectedEOF
	}
	payload := p.b[p.pos : p.pos+int(n)-4]
	p.pos += int(n) - 4
	return payload, "", nil
}

// readPktLines splits a whole pkt-line stream into payload lines.
func readPktLines(b []byte) ([][]byte, error) {
	var out [][]byte
	r := &pktReader{b: b}
	for {
		payload, ctrl, err := r.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if ctrl != "" {
			out = append(out, []byte("\x00"+ctrl))
			continue
		}
		out = append(out, payload)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pack files
// ---------------------------------------------------------------------------

// readPack parses a pack file: verifies the trailer and returns the objects
// in pack order. Delta objects are rejected (the clients never send them).
func readPack(pack []byte) ([]*gitObject, error) {
	if len(pack) < 12+20 {
		return nil, fmt.Errorf("pack too short")
	}
	if string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack signature")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	// verify trailer
	want := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(want[:], pack[len(pack)-20:]) {
		return nil, fmt.Errorf("pack trailer mismatch")
	}
	body := pack[12 : len(pack)-20]
	pos := 0
	objs := make([]*gitObject, 0, count)
	for i := uint32(0); i < count; i++ {
		if pos >= len(body) {
			return nil, fmt.Errorf("pack truncated at object %d", i)
		}
		// varint header: first byte holds type (bits 4-6) and low 4 size bits
		b := body[pos]
		pos++
		typ := (b >> 4) & 0x7
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(body) {
				return nil, fmt.Errorf("pack truncated in object header")
			}
			b = body[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		switch typ {
		case 6: // ofs-delta
			return nil, fmt.Errorf("delta objects not supported")
		case 7: // ref-delta
			return nil, fmt.Errorf("delta objects not supported")
		}
		// zlib stream: inflate, tracking consumed bytes via a counting reader
		cr := &countingReader{r: bytes.NewReader(body[pos:])}
		zr, err := zlib.NewReader(cr)
		if err != nil {
			return nil, fmt.Errorf("zlib open: %w", err)
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("zlib inflate: %w", err)
		}
		if int64(len(data)) != size {
			return nil, fmt.Errorf("object size mismatch: got %d want %d", len(data), size)
		}
		pos += cr.n
		objs = append(objs, &gitObject{typ: byte(typ), data: data})
	}
	return objs, nil
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// ReadByte makes flate read exactly one byte at a time instead of wrapping
// this reader in a bufio buffer (which would over-read past the zlib stream
// and corrupt the byte count).
func (c *countingReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := c.Read(b[:])
	return b[0], err
}

// writePack encodes objects as a pack file (plain objects, zlib level 1).
// The trailer is the SHA-1 of the whole pack content before the trailer.
func writePack(objs []*gitObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objs)))
	buf.Write(header)
	h := sha1.New()
	h.Write(header)
	for _, o := range objs {
		enc := encodeObject(o)
		buf.Write(enc)
		h.Write(enc)
	}
	sum := h.Sum(nil)
	buf.Write(sum)
	return buf.Bytes()
}

func encodeObject(o *gitObject) []byte {
	var out []byte
	typ := o.typ
	size := len(o.data)
	// first byte: type in bits 4-6, low 4 bits of size
	first := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	out = append(out, first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	var zb bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zb, 1)
	zw.Write(o.data)
	zw.Close()
	out = append(out, zb.Bytes()...)
	return out
}

// sidebandWrap wraps data into sideband band-1 packets (max 65515 data bytes
// per packet), then a flush.
func sidebandWrap(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		chunk := data[:n]
		data = data[n:]
		payload := make([]byte, 1+len(chunk))
		payload[0] = 1
		copy(payload[1:], chunk)
		out.Write(pktLine(payload))
	}
	out.WriteString(pktFlush)
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// Object graph helpers for fetch
// ---------------------------------------------------------------------------

// fetchClosure computes the objects a fetch must deliver.
//   wants: commit/tag oids the client wants
//   haves: commit oids the client already has
//   shallowBoundaries: oids the client treats as shallow cuts
//   depth: 0 = unlimited, N = include N generations from wants
// Returns (objects in deterministic order, shallow list, unshallow list).
func (r *repo) fetchClosure(wants, haves, shallowBoundaries []string, depth int) (objs []*gitObject, shallow, unshallow []string) {
	// Exclusion set: closure of haves, never traversing past a shallow
	// boundary commit's parents.
	shallowSet := make(map[string]bool, len(shallowBoundaries))
	for _, s := range shallowBoundaries {
		shallowSet[s] = true
	}
	excluded := make(map[string]bool)
	var walk func(oid string, isCommit bool)
	var walkCommit func(oid string)
	walkCommit = func(oid string) {
		if excluded[oid] {
			return
		}
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			return
		}
		excluded[oid] = true
		tree, parents := parseCommitHeader(obj.data)
		walk(tree, false)
		if shallowSet[oid] {
			return // do not traverse past a shallow boundary
		}
		for _, p := range parents {
			walkCommit(p)
		}
	}
	walk = func(oid string, isCommit bool) {
		if excluded[oid] {
			return
		}
		obj, ok := r.objects[oid]
		if !ok {
			return
		}
		switch obj.typ {
		case objCommit:
			walkCommit(oid)
		case objTag:
			excluded[oid] = true
			target := tagTarget(obj.data)
			if target != "" {
				walk(target, true)
			}
		case objTree:
			excluded[oid] = true
			for _, e := range parseTree(obj.data) {
				walk(e.oid, false)
			}
		default:
			excluded[oid] = true
		}
	}
	for _, h := range haves {
		walk(h, true)
	}

	// Inclusion walk from wants with optional depth limit. The walk traverses
	// the commit graph even through already-held commits so that a deepen
	// fetch can reach the new generations below a held tip.
	included := make(map[string]bool)
	visited := make(map[string]bool)
	var order []string
	var includeTree func(oid string)
	var includeCommit func(oid string, gen int)
	includeTree = func(oid string) {
		if included[oid] || excluded[oid] {
			return
		}
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objTree {
			return
		}
		included[oid] = true
		order = append(order, oid)
		for _, e := range parseTree(obj.data) {
			if e.mode == "40000" {
				includeTree(e.oid)
			} else {
				if !included[e.oid] && !excluded[e.oid] {
					if _, ok := r.objects[e.oid]; ok {
						included[e.oid] = true
						order = append(order, e.oid)
					}
				}
			}
		}
	}
	includeCommit = func(oid string, gen int) {
		if visited[oid] {
			return
		}
		visited[oid] = true
		obj, ok := r.objects[oid]
		if !ok {
			return
		}
		if obj.typ == objTag {
			if !excluded[oid] {
				included[oid] = true
				order = append(order, oid)
			}
			target := tagTarget(obj.data)
			if target != "" {
				includeCommit(target, gen)
			}
			return
		}
		if obj.typ != objCommit {
			return
		}
		tree, parents := parseCommitHeader(obj.data)
		if !excluded[oid] {
			included[oid] = true
			order = append(order, oid)
			includeTree(tree)
			// boundary: this commit is at the depth limit and has parents
			if depth > 0 && gen+1 >= depth && len(parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
		if depth > 0 && gen+1 >= depth {
			return
		}
		for _, p := range parents {
			includeCommit(p, gen+1)
		}
	}
	for _, w := range wants {
		includeCommit(w, 0)
	}

	// unshallow: previously-shallow boundaries whose parent is now included
	for _, s := range shallowBoundaries {
		obj, ok := r.objects[s]
		if ok && obj.typ == objCommit {
			_, parents := parseCommitHeader(obj.data)
			for _, p := range parents {
				if included[p] {
					unshallow = append(unshallow, s)
					break
				}
			}
		}
	}
	shallow = dedupStr(shallow)
	unshallow = dedupStr(unshallow)

	for _, oid := range order {
		if o, ok := r.objects[oid]; ok {
			objs = append(objs, o)
		}
	}
	return objs, shallow, unshallow
}

func tagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return line[7:]
		}
		if line == "" {
			break
		}
	}
	return ""
}

func dedupStr(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
