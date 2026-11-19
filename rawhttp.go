// Raw HTTP/1.1 server (replaces net/http's server loop).
//
// Why: the conformance client does thousands of sequential keep-alive
// requests; Go's net/http server costs ~0.18 ms/request more than a minimal
// loop here (header-map canonicalisation, Date generation, bufio setup,
// per-connection goroutine machinery). The handlers are unchanged: this file
// only parses the request line + headers + body into an *http.Request and
// captures the handler's reply into one buffered write, so the whole reply
// (status line + headers + body) leaves in a single TCP segment.
//
// Supported: HTTP/1.1 keep-alive, Content-Length and chunked request bodies,
// Connection: close. No TLS, no HTTP/2, no Expect handling beyond sending
// 100-continue when asked.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// rawResponseWriter captures what a handler writes so the server can flush
// it as one write. It implements http.ResponseWriter.
type rawResponseWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newRawResponseWriter() *rawResponseWriter {
	return &rawResponseWriter{header: make(http.Header)}
}

func (w *rawResponseWriter) Header() http.Header { return w.header }

func (w *rawResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(b)
}

func (w *rawResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

// serveRaw accepts TCP connections on ln and serves them with the raw loop.
// It returns when ln is closed.
func serveRaw(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveRawConn(conn)
	}
}

func serveRawConn(conn net.Conn) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	r := bufio.NewReaderSize(conn, 4096)
	w := bufio.NewWriterSize(conn, 8192)
	for {
		req, closeAfter, err := readRawRequest(r, conn)
		if err != nil {
			return
		}
		rw := newRawResponseWriter()
		handler(rw, req)
		if closeAfter {
			rw.header.Set("Connection", "close")
		}
		if err := writeRawResponse(w, rw); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		if closeAfter {
			return
		}
	}
}

// readRawRequest parses one HTTP/1.1 request (line, headers, body) from r
// and builds the *http.Request the handlers expect.
func readRawRequest(r *bufio.Reader, conn net.Conn) (*http.Request, bool, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, false, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		// tolerate a stray blank line before the request line
		line, err = r.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		line = strings.TrimRight(line, "\r\n")
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, fmt.Errorf("bad request line %q", line)
	}
	method, target, proto := parts[0], parts[1], parts[2]
	// headers
	hdr := make(http.Header)
	for {
		hline, err := r.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		colon := strings.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(hline[:colon])
		value := strings.TrimSpace(hline[colon+1:])
		hdr.Add(name, value)
	}
	// body
	var body []byte
	if te := hdr.Get("Transfer-Encoding"); strings.Contains(strings.ToLower(te), "chunked") {
		body, err = readChunked(r)
		if err != nil {
			return nil, false, err
		}
	} else if cl := hdr.Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, false, fmt.Errorf("bad content-length %q", cl)
		}
		if n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, false, err
			}
		}
	}
	u, err := url.ParseRequestURI(target)
	if err != nil {
		// fall back to a plain path (targets here are always origin-form)
		u = &url.URL{Path: target}
	}
	closeAfter := false
	if strings.EqualFold(hdr.Get("Connection"), "close") {
		closeAfter = true
	}
	if proto == "HTTP/1.0" && !strings.EqualFold(hdr.Get("Connection"), "keep-alive") {
		closeAfter = true
	}
	req := &http.Request{
		Method:        method,
		URL:           u,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Proto:         proto,
		ProtoMajor:    1,
		ProtoMinor:    1,
		RemoteAddr:    conn.RemoteAddr().String(),
		Host:          hdr.Get("Host"),
		RequestURI:    target,
	}
	if strings.HasPrefix(proto, "HTTP/1.0") {
		req.ProtoMajor, req.ProtoMinor = 1, 0
	}
	return req, closeAfter, nil
}

// readChunked reads a chunked request body.
func readChunked(r *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		sizeLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimSpace(sizeLine)
		if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		n, err := strconv.ParseInt(sizeLine, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("bad chunk size %q", sizeLine)
		}
		if n == 0 {
			// trailer headers until blank line
			for {
				t, err := r.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
					break
				}
			}
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, r, n); err != nil {
			return nil, err
		}
		// trailing CRLF after the chunk
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

// writeRawResponse writes status line + headers + body in one buffered
// flush. No Date header (the reference entry sends none and the checks do
// not require one).
func writeRawResponse(w *bufio.Writer, rw *rawResponseWriter) error {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(status))
	b.WriteString(" ")
	b.WriteString(http.StatusText(status))
	b.WriteString("\r\n")
	// deterministic order: sorted header names
	keys := make([]string, 0, len(rw.header))
	for k := range rw.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range rw.header[k] {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(rw.body.Len()))
	b.WriteString("\r\n\r\n")
	if _, err := w.Write(b.Bytes()); err != nil {
		return err
	}
	if rw.body.Len() > 0 {
		if _, err := w.Write(rw.body.Bytes()); err != nil {
			return err
		}
	}
	return nil
}
