package core

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestDAV(t *testing.T) {
	s := NewSession(DefaultLimits)
	if _, err := s.DAV("PROPFIND", "/", nil, nil); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("no image: %v", err)
	}
	img, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatal(err)
	}
	l, err := s.List("/")
	if err != nil {
		t.Fatal(err)
	}
	var file Entry
	for _, e := range l.Entries {
		if e.Kind == "file" {
			file = e
			break
		}
	}

	a, err := s.DAV("PROPFIND", "/", [][2]string{{"Depth", "1"}}, nil)
	if err != nil || a.Status != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: %d, %v", a.Status, err)
	}
	if !strings.Contains(string(a.Body), file.Name) {
		t.Fatalf("the listing does not name %s:\n%s", file.Name, a.Body)
	}

	a, err = s.DAV("GET", "/"+file.Name, nil, nil)
	want, _, _ := s.Read("/"+file.Name, 0, file.Size)
	if err != nil || a.Status != http.StatusOK || !bytes.Equal(a.Body, want) {
		t.Fatalf("GET: %d, %v, %d bytes, want %d", a.Status, err, len(a.Body), len(want))
	}

	// Read-only: the handler refuses a write.
	a, err = s.DAV("PUT", "/new.txt", nil, []byte("x"))
	if err != nil || a.Status < 400 {
		t.Fatalf("PUT on a read-only export: %d, %v", a.Status, err)
	}

	for _, c := range []struct{ method, target string }{
		{"TRACE", "/"},
		{"GET", "relative"},
		{"GET", "/a?b"},
		{"GET", "/a\x00b"},
		{"GET", "/" + strings.Repeat("a", 9000)},
	} {
		if _, err := s.DAV(c.method, c.target, nil, nil); err == nil {
			t.Errorf("%s %q was served", c.method, c.target)
		}
	}
	if _, err := s.DAV("PUT", "/x", nil, make([]byte, MaxDAV+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized body: %v", err)
	}

	// A new image gets a new handler.
	s.Close()
	if _, err := s.DAV("PROPFIND", "/", nil, nil); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestRecorderBound(t *testing.T) {
	r := &recorder{h: http.Header{}}
	if _, err := r.Write(make([]byte, MaxDAV)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte{1}); !errors.Is(err, ErrTooLargeAnswer) || !r.over {
		t.Fatalf("over the bound: %v", err)
	}
	r.WriteHeader(500)
	if r.status != http.StatusOK {
		t.Fatalf("status rewritten to %d", r.status)
	}
}
