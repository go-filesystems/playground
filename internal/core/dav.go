package core

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-filesystems/webdav"
)

// The explorer (go-fileshare/explorer), embedded by the page, browses the
// open image over WebDAV: its requests come through the page's bridge to
// DAV, which answers them with go-filesystems/webdav's handler, read-only,
// inside the tab. Nothing goes over a network.

// MaxDAV bounds a WebDAV request body and answer: the explorer's upload
// piece, or a listing of MaxEntries.
const MaxDAV = 16 << 20

// davMethods are the methods passed to the handler; davReading those that
// change nothing.
var (
	davMethods = []string{"OPTIONS", "PROPFIND", "GET", "HEAD", "PUT", "MKCOL", "DELETE", "MOVE", "COPY", "PROPPATCH", "LOCK", "UNLOCK"}
	davReading = []string{"OPTIONS", "PROPFIND", "GET", "HEAD"}
)

// ErrTooLargeAnswer is a WebDAV answer over MaxDAV.
var ErrTooLargeAnswer = errors.New("the answer is larger than the page takes")

// DAVAnswer is the handler's answer to one request.
type DAVAnswer struct {
	Status int         `json:"status"`
	Header [][2]string `json:"headers"`
	Body   []byte      `json:"-"`
}

// DAV answers one WebDAV request about the open image: method and target
// (an absolute path, escaped, as the explorer sends it), its headers and
// body.
func (s *Session) DAV(method, target string, header [][2]string, body []byte) (a DAVAnswer, err error) {
	if !slices.Contains(davMethods, method) {
		return a, fmt.Errorf("method %q is not served", method)
	}
	if target == "" || target[0] != '/' || len(target) > 8<<10 || strings.ContainsAny(target, "\x00?#") {
		return a, ErrBadPath
	}
	if len(body) > MaxDAV || len(header) > 100 {
		return a, ErrTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fs == nil {
		return a, ErrNotOpen
	}
	if s.dav == nil {
		// Read-write: a write reaches the driver, then the COW under it,
		// in memory; the dropped file is never written.
		h, err := webdav.New(s.fs, webdav.ReadWrite())
		if err != nil {
			return a, err
		}
		s.dav = h
	}
	if !slices.Contains(davReading, method) {
		// The file a page's read keeps whole may be the one this changes.
		s.whole, s.wholePath = nil, ""
	}
	req, err := http.NewRequest(method, "http://image"+target, bytes.NewReader(body))
	if err != nil {
		return a, ErrBadPath
	}
	for _, kv := range header {
		req.Header.Add(kv[0], kv[1])
	}
	req.RemoteAddr = "127.0.0.1:0" // the tab itself
	rec := &recorder{h: http.Header{}}
	defer guard(&err)
	s.dav.ServeHTTP(rec, req)
	if rec.over {
		return DAVAnswer{}, ErrTooLargeAnswer
	}
	a = DAVAnswer{Status: rec.status, Body: rec.buf.Bytes()}
	if a.Status == 0 {
		a.Status = http.StatusOK
	}
	for k, vs := range rec.h {
		for _, v := range vs {
			a.Header = append(a.Header, [2]string{k, v})
		}
	}
	return a, nil
}

// recorder is an http.ResponseWriter that keeps at most MaxDAV bytes.
type recorder struct {
	h      http.Header
	status int
	buf    bytes.Buffer
	over   bool
}

func (r *recorder) Header() http.Header { return r.h }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if r.over || r.buf.Len()+len(p) > MaxDAV {
		r.over = true
		return 0, ErrTooLargeAnswer
	}
	return r.buf.Write(p)
}
