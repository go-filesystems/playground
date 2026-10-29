package core

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"testing"
)

// TestCOWAgainstAModel writes and reads at random against a plain byte slice
// written the same way: the COW must read back exactly what the model holds,
// and never write its base.
func TestCOWAgainstAModel(t *testing.T) {
	const size = 5*cowBlock + 123 // a short last block
	base := make([]byte, size)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range base {
		base[i] = byte(rng.IntN(256))
	}
	orig := bytes.Clone(base)
	model := bytes.Clone(base)
	c := NewCOW(bytes.NewReader(base), size, MaxChanges)
	for range 2000 {
		off := rng.Int64N(size)
		n := rng.Int64N(min(size-off, 3*cowBlock) + 1)
		if rng.IntN(2) == 0 {
			p := make([]byte, n)
			for i := range p {
				p[i] = byte(rng.IntN(256))
			}
			if got, err := c.WriteAt(p, off); err != nil || got != len(p) {
				t.Fatalf("WriteAt(%d, %d) = %d, %v", n, off, got, err)
			}
			copy(model[off:], p)
		} else {
			p := make([]byte, n)
			got, err := c.ReadAt(p, off)
			if got != len(p) || (err != nil && err != io.EOF) {
				t.Fatalf("ReadAt(%d, %d) = %d, %v", n, off, got, err)
			}
			if !bytes.Equal(p, model[off:off+n]) {
				t.Fatalf("ReadAt(%d, %d) differs from the model", n, off)
			}
		}
	}
	whole := make([]byte, size)
	if _, err := c.ReadAt(whole, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(whole, model) {
		t.Fatal("the whole image differs from the model")
	}
	if !bytes.Equal(base, orig) {
		t.Fatal("the base was written")
	}
}

func TestCOWBounds(t *testing.T) {
	base := make([]byte, 3*cowBlock)
	c := NewCOW(bytes.NewReader(base), int64(len(base)), cowBlock) // room for one block
	if _, err := c.WriteAt([]byte{1}, int64(len(base))); !errors.Is(err, ErrPastEnd) {
		t.Fatalf("past the end: %v", err)
	}
	if _, err := c.WriteAt([]byte{1}, -1); !errors.Is(err, ErrNegativeOffset) {
		t.Fatalf("negative: %v", err)
	}
	if _, err := c.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteAt([]byte{2}, 10); err != nil {
		t.Fatalf("the same block again: %v", err)
	}
	// A write spanning a written block and a new one is refused whole.
	if _, err := c.WriteAt(make([]byte, 2), cowBlock-1); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("over the bound: %v", err)
	}
	if got := c.Changed(); got != cowBlock {
		t.Fatalf("Changed = %d", got)
	}
	p := make([]byte, 4)
	if n, err := c.ReadAt(p, int64(len(base))-2); n != 2 || err != io.EOF {
		t.Fatalf("a read across the end: %d, %v", n, err)
	}
	if _, err := c.ReadAt(p, -1); !errors.Is(err, ErrNegativeOffset) {
		t.Fatalf("negative read: %v", err)
	}
}

// The modified image, read back whole, opens again with the change in it:
// what the page downloads is a valid image of the same format.
func TestModifiedImageRoundTrip(t *testing.T) {
	img, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(DefaultLimits)
	if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatal(err)
	}
	put := func(path, body string) {
		t.Helper()
		if a, err := s.DAV("PUT", path, nil, []byte(body)); err != nil || a.Status >= 300 {
			t.Fatalf("PUT %s: %d, %v", path, a.Status, err)
		}
	}
	if a, err := s.DAV("MKCOL", "/made", nil, nil); err != nil || a.Status >= 300 {
		t.Fatalf("MKCOL: %d, %v", a.Status, err)
	}
	put("/made/hello.txt", "hello from the tab")
	if a, err := s.DAV("DELETE", "/README.TXT", nil, nil); err != nil || a.Status >= 300 {
		t.Fatalf("DELETE: %d, %v", a.Status, err)
	}

	out := downloadImage(t, s)
	if p := os.Getenv("CORE_MODIFIED_IMAGE"); p != "" {
		// For a checker nobody here wrote: fsck_msdos -n, fsck.vfat -n.
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(out) != len(img) {
		t.Fatalf("the modified image is %d bytes, the original %d", len(out), len(img))
	}
	again := NewSession(DefaultLimits)
	info, err := again.Open(bytes.NewReader(out), int64(len(out)))
	if err != nil || info.Type != "fat32" {
		t.Fatalf("reopening: %+v, %v", info, err)
	}
	if got, _, err := again.Read("/made/hello.txt", 0, 100); err != nil || string(got) != "hello from the tab" {
		t.Fatalf("the written file: %q, %v", got, err)
	}
	if _, _, err := again.Read("/README.TXT", 0, 10); err == nil {
		t.Fatal("the deleted file is still there")
	}
}

// Inside LUKS, a change is encrypted into the image: the modified image
// needs the password, and holds the change.
func TestModifiedLUKSRoundTrip(t *testing.T) {
	img := luksSample(t, "pw")
	s := NewSession(DefaultLimits)
	if _, err := s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", []byte("pw")); err != nil {
		t.Fatal(err)
	}
	if a, err := s.DAV("PUT", "/secret.txt", nil, []byte("encrypted on the way out")); err != nil || a.Status >= 300 {
		t.Fatalf("PUT: %d, %v", a.Status, err)
	}
	out := downloadImage(t, s)
	if bytes.Contains(out, []byte("encrypted on the way out")) {
		t.Fatal("the change is in the image in clear")
	}
	again := NewSession(DefaultLimits)
	if _, err := again.Open(bytes.NewReader(out), int64(len(out))); !errors.Is(err, ErrPassword) {
		t.Fatalf("no password: %v", err)
	}
	if _, err := again.OpenWith(bytes.NewReader(out), int64(len(out)), "auto", []byte("pw")); err != nil {
		t.Fatal(err)
	}
	if got, _, err := again.Read("/secret.txt", 0, 100); err != nil || string(got) != "encrypted on the way out" {
		if got, _, err = again.Read("/SECRET.TXT", 0, 100); err != nil || string(got) != "encrypted on the way out" {
			t.Fatalf("the written file: %q, %v", got, err)
		}
	}
}

// The explorer uploads in pieces (go-fileshare/explorer internal/dav): an
// empty PUT of a part file, each piece appended with a Content-Range, then a
// MOVE that refuses to replace. That sequence must work over the COW.
func TestExplorerUploadSequence(t *testing.T) {
	img, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(DefaultLimits)
	if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 300<<10) // three pieces of 128 KiB, the last short
	for i := range data {
		data[i] = byte(i * 7)
	}
	do := func(method, path string, hs [][2]string, body []byte) int {
		t.Helper()
		a, err := s.DAV(method, path, hs, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return a.Status
	}
	if st := do("PUT", "/big.bin.part", nil, []byte{}); st >= 300 {
		t.Fatalf("empty PUT: %d", st)
	}
	const piece = 128 << 10
	for off := 0; off < len(data); off += piece {
		end := min(off+piece, len(data))
		cr := "bytes " + itoa(off) + "-" + itoa(end-1) + "/" + itoa(len(data))
		if st := do("PUT", "/big.bin.part", [][2]string{{"Content-Range", cr}}, data[off:end]); st >= 300 {
			t.Fatalf("piece at %d: %d", off, st)
		}
	}
	if st := do("MOVE", "/big.bin.part", [][2]string{{"Destination", "/big.bin"}, {"Overwrite", "F"}}, nil); st >= 300 {
		t.Fatalf("MOVE: %d", st)
	}
	out := downloadImage(t, s)
	again := NewSession(DefaultLimits)
	if _, err := again.Open(bytes.NewReader(out), int64(len(out))); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for off := int64(0); off < int64(len(data)); {
		b, _, err := again.Read("/big.bin", off, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			break
		}
		got = append(got, b...)
		off += int64(len(b))
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the uploaded file reads back %d bytes, differing", len(got))
	}
}

// downloadImage reads the modified image as the page does, in MaxChunk
// pieces through the protocol.
func downloadImage(t *testing.T, s *Session) []byte {
	t.Helper()
	var out []byte
	for {
		resp, data := s.Handle([]byte(`{"op":"image","off":` + itoa(len(out)) + `,"n":4194304}`))
		if !bytes.Contains(resp, []byte(`"ok":true`)) {
			t.Fatalf("image: %s", resp)
		}
		if len(data) == 0 {
			return out
		}
		out = append(out, data...)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
