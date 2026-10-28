package core

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func handle(t *testing.T, s *Session, req string) (Response, []byte) {
	t.Helper()
	b, data := s.Handle([]byte(req))
	var r Response
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("response %s is not JSON: %v", b, err)
	}
	return r, data
}

func TestHandle(t *testing.T) {
	s := NewSession(DefaultLimits)
	r, img := handle(t, s, `{"op":"sample","cache":false}`)
	if !r.OK || r.Info == nil || r.Info.Type != "fat32" || r.Info.Cached || len(img) != SampleSize {
		t.Fatalf("sample: %+v (%d bytes)", r, len(img))
	}
	r, _ = handle(t, s, `{"op":"sample","cache":true}`)
	if !r.Info.Cached {
		t.Fatal("cache flag ignored")
	}
	r, _ = handle(t, s, `{"op":"list","path":"/docs"}`)
	if !r.OK || r.Listing == nil || len(r.Listing.Entries) != 3 {
		t.Fatalf("list: %+v", r)
	}
	r, data := handle(t, s, `{"op":"read","path":"/docs/hello.txt","n":5}`)
	if !r.OK || string(data) != "Hello" || r.Size == 0 {
		t.Fatalf("read: %+v %q", r, data)
	}
	r, _ = handle(t, s, `{"op":"stats"}`)
	if !r.OK || r.Stats == nil || r.Stats.BackendReads == 0 {
		t.Fatalf("stats: %+v", r)
	}
	r, _ = handle(t, s, `{"op":"info"}`)
	if !r.OK || r.Info.Label != "PLAYGROUND" {
		t.Fatalf("info: %+v", r)
	}
	r, _ = handle(t, s, `{"op":"read","path":"/nope"}`)
	if r.OK || r.Error == "" {
		t.Fatalf("missing read: %+v", r)
	}
	r, _ = handle(t, s, `{"op":"list","path":"/nope"}`)
	if r.OK {
		t.Fatalf("missing list: %+v", r)
	}
	r, _ = handle(t, s, `{"op":"close"}`)
	if !r.OK {
		t.Fatal("close")
	}
	for _, bad := range []string{``, `{`, `[]`, `{"op":"list","evil":1}`, `{"op":"list"} {}`, `{"op":"rm"}`,
		`{"op":"list","path":"/"}`, `{"op":"` + strings.Repeat("x", MaxRequest) + `"}`} {
		r, data := handle(t, s, bad)
		if r.OK || r.Error == "" || data != nil {
			t.Fatalf("%.40q: %+v", bad, r)
		}
	}
	ok := OpenResponse(Info{Type: "fat32"}, nil, 1.5)
	if !bytes.Contains(ok, []byte(`"ok":true`)) {
		t.Fatalf("OpenResponse ok: %s", ok)
	}
	fail := OpenResponse(Info{Type: "ext4"}, errTest("bad"), 1)
	if !bytes.Contains(fail, []byte(`"error":"bad"`)) || !bytes.Contains(fail, []byte(`"ext4"`)) {
		t.Fatalf("OpenResponse error: %s", fail)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

// FuzzHandle: whatever the page (or anything that can post to the worker)
// sends, the engine answers with JSON and does not panic.
func FuzzHandle(f *testing.F) {
	for _, seed := range []string{
		`{"op":"list","path":"/"}`, `{"op":"read","path":"/docs/lines.txt","off":100,"n":4096}`,
		`{"op":"read","path":"/README.TXT","off":-5,"n":-1}`, `{"op":"stats"}`, `{"op":"info"}`,
		`{"op":"list","path":"/../../etc"}`, `{"op":"read","path":"/docs","n":9223372036854775807}`,
	} {
		f.Add([]byte(seed))
	}
	img, err := Sample()
	if err != nil {
		f.Fatal(err)
	}
	s := NewSession(DefaultLimits)
	f.Fuzz(func(t *testing.T, req []byte) {
		if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(req, []byte(`"sample"`)) {
			return // formats a new image: correct, and too slow to fuzz
		}
		b, data := s.Handle(req)
		var r Response
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("not JSON: %s", b)
		}
		if int64(len(data)) > DefaultLimits.MaxChunk {
			t.Fatalf("returned %d bytes, over the chunk limit", len(data))
		}
	})
}
