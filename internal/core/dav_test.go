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

	// A write lands in the COW: visible to the page's own reads, counted,
	// and the dropped image untouched.
	before := bytes.Clone(img)
	a, err = s.DAV("PUT", "/new.txt", nil, []byte("written in the tab"))
	if err != nil || a.Status >= 300 {
		t.Fatalf("PUT: %d, %v: %s", a.Status, err, a.Body)
	}
	if got, _, err := s.Read("/NEW.TXT", 0, 100); err != nil || string(got) != "written in the tab" {
		if got, _, err = s.Read("/new.txt", 0, 100); err != nil || string(got) != "written in the tab" {
			t.Fatalf("reading it back: %q, %v", got, err)
		}
	}
	if st := s.Stats(); st.Changed == 0 {
		t.Fatal("no change counted")
	}
	if !bytes.Equal(before, img) {
		t.Fatal("the dropped image was written")
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

// FuzzDAV sends arbitrary WebDAV requests about the sample, as a page's
// bridge could: no panic may escape, no answer may pass MaxDAV, and the
// read-only export must not change the image.
func FuzzDAV(f *testing.F) {
	img, err := Sample()
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []struct{ method, target, header, body string }{
		{"PROPFIND", "/", "Depth: 1", propfindSeed},
		{"PROPFIND", "/docs/", "Depth: infinity", ""},
		{"GET", "/README.TXT", "Range: bytes=10-5", ""},
		{"GET", "/%2e%2e/%2e%2e/etc/passwd", "", ""},
		{"PUT", "/README.TXT", "Content-Range: bytes 0-3/4", "abcd"},
		{"MOVE", "/README.TXT", "Destination: /x", ""},
		{"LOCK", "/", "Timeout: Second-1", "<x/>"},
	} {
		f.Add(s.method, s.target, s.header, []byte(s.body))
	}
	f.Fuzz(func(t *testing.T, method, target, header string, body []byte) {
		before := bytes.Clone(img)
		s := NewSession(DefaultLimits)
		if _, err := s.Open(bytes.NewReader(before), int64(len(before))); err != nil {
			t.Fatal(err)
		}
		var hs [][2]string
		if k, v, ok := strings.Cut(header, ":"); ok {
			hs = append(hs, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
		}
		a, err := s.DAV(method, target, hs, body)
		if err == nil && len(a.Body) > MaxDAV {
			t.Fatalf("%d bytes answered", len(a.Body))
		}
		if !bytes.Equal(before, img) {
			t.Fatal("a request wrote the dropped image, not the COW")
		}
	})
}

const propfindSeed = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`

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
