package core

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"testing"

	filesystem_ext4 "github.com/go-filesystems/ext4"
	filesystem_fat32 "github.com/go-filesystems/fat32"
)

func smallImage(t *testing.T, name string) []byte {
	t.Helper()
	f, err := smallFAT.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func openBytes(t *testing.T, img []byte) *Session {
	t.Helper()
	s := NewSession(DefaultLimits)
	if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatal(err)
	}
	return s
}

// tree lists every file of the open image with a hash of its bytes.
func tree(t *testing.T, s *Session, dir string, out map[string]string) {
	t.Helper()
	l, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Entries {
		p := path.Join(dir, e.Name)
		if e.Kind == "dir" {
			out[p] = "dir"
			tree(t, s, p, out)
			continue
		}
		b, _, err := s.Read(p, 0, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
}

func fetchCopy(t *testing.T, s *Session) []byte {
	t.Helper()
	var out []byte
	for {
		resp, data := s.Handle([]byte(`{"op":"copyimage","off":` + itoa(len(out)) + `,"n":4194304}`))
		if !bytes.Contains(resp, []byte(`"ok":true`)) {
			t.Fatalf("copyimage: %s", resp)
		}
		if len(data) == 0 {
			return out
		}
		out = append(out, data...)
	}
}

// A small FAT image becomes a bigger one, of another type when asked, with the
// same tree; the source is the same bytes after, by hash.
func TestCopyResizedGrowsAndRetypes(t *testing.T) {
	for _, name := range []string{"fat12.img.gz", "fat16.img.gz"} {
		img := smallImage(t, name)
		before := sha256.Sum256(img)
		s := openBytes(t, img)
		want := map[string]string{}
		tree(t, s, "/", want)
		for _, c := range []struct {
			size int64
			kind string
			want string
		}{
			{16 << 20, "fat12", "fat12"},
			{40 << 20, "fat16", "fat16"},
			{64 << 20, "fat32", "fat32"},
			{64 << 20, "auto", "fat16"},
		} {
			info, err := s.CopyResized(c.size, c.kind, "")
			if err != nil || info.Type != c.want || info.Size != c.size {
				t.Fatalf("%s -> %d %s: %+v, %v", name, c.size, c.kind, info, err)
			}
			out := fetchCopy(t, s)
			if int64(len(out)) != c.size {
				t.Fatalf("%s: downloaded %d bytes of %d", name, len(out), c.size)
			}
			again := openBytes(t, out)
			if again.Info().Type != c.want {
				t.Fatalf("%s: reopened as %s", name, again.Info().Type)
			}
			got := map[string]string{}
			tree(t, again, "/", got)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s -> %s: the tree changed:\n%v\n%v", name, c.kind, got, want)
			}
		}
		if after := sha256.Sum256(img); after != before {
			t.Fatalf("%s: the source image changed", name)
		}
	}
}

// The copy carries the changes made in the tab, and a label when given.
func TestCopyResizedCarriesChangesAndLabel(t *testing.T) {
	s, img := openSample(t, DefaultLimits)
	before := sha256.Sum256(img)
	if a, err := s.DAV("PUT", "/new.txt", nil, []byte("made in the tab")); err != nil || a.Status >= 300 {
		t.Fatalf("PUT: %d, %v", a.Status, err)
	}
	info, err := s.CopyResized(32<<20, "fat16", "BIGGER")
	if err != nil || info.Type != "fat16" || info.Label != "BIGGER" {
		t.Fatalf("%+v, %v", info, err)
	}
	again := openBytes(t, fetchCopy(t, s))
	if b, _, err := again.Read("/new.txt", 0, 100); err != nil || string(b) != "made in the tab" {
		t.Fatalf("the change: %q, %v", b, err)
	}
	if after := sha256.Sum256(img); after != before {
		t.Fatal("the dropped image changed")
	}
}

// Every refusal says why, and none touches the open image or leaves a copy.
func TestCopyResizedRefusals(t *testing.T) {
	// A root with more entries than a FAT12/16 fixed root holds.
	m := &memImage{b: make([]byte, 16<<20)}
	wide, err := filesystem_fat32.FormatImage(m, int64(len(m.b)), filesystem_fat32.FormatConfig{Label: "WIDE"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if err := wide.WriteFile(fmt.Sprintf("/F%04d.TXT", i), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wide.Close()
	wideSession := openBytes(t, m.b)
	wideHash := sha256.Sum256(m.b)

	sample, simg := openSample(t, DefaultLimits)
	sampleHash := sha256.Sum256(simg)

	if _, err := wideSession.CopyResized(16<<20, "fat16", ""); !errors.Is(err, filesystem_fat32.ErrCopyRootFull) {
		t.Fatalf("a wide root into FAT16: %v", err)
	}
	if _, err := sample.CopyResized(128*ResizeSector, "auto", ""); err == nil {
		t.Fatal("a 64 KiB image holds the sample")
	} else if !errors.Is(err, filesystem_fat32.ErrCopyNoFit) {
		t.Fatalf("the sample into 64 KiB: %v", err)
	}
	// A type the size cannot take: FAT32 needs 65525 clusters.
	if _, err := sample.CopyResized(4<<20, "fat32", ""); err == nil || !strings.Contains(err.Error(), "cannot be formatted") {
		t.Fatalf("FAT32 in 4 MiB: %v", err)
	}
	if _, _, err := sample.ReadCopy(0, 10); err == nil {
		t.Fatal("a refused copy left bytes to read")
	}
	if wideHash != sha256.Sum256(m.b) || sampleHash != sha256.Sum256(simg) {
		t.Fatal("a refusal changed the open image")
	}
	// The open image still works after the refusals.
	if _, err := sample.List("/"); err != nil {
		t.Fatal(err)
	}

	// No image.
	if _, err := NewSession(DefaultLimits).CopyResized(1<<20, "auto", ""); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("no image: %v", err)
	}
}

func TestCopyResizedNotFAT(t *testing.T) {
	p := t.TempDir() + "/e.img"
	e, err := filesystem_ext4.Format(p, 16<<20, filesystem_ext4.FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	img, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := openBytes(t, img)
	if _, err := s.CopyResized(32<<20, "auto", ""); !errors.Is(err, ErrNotFAT) {
		t.Fatalf("ext4: %v", err)
	}
}

// The size is typed by a person: it is bounded before any memory is taken.
func TestCopyResizedSizeIsBounded(t *testing.T) {
	s, _ := openSample(t, DefaultLimits)
	for _, size := range []int64{0, -512, 1, 511, 513, MaxResize + ResizeSector, 1 << 40, 1<<63 - 1, -1 << 63} {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		_, err := s.CopyResized(size, "auto", "")
		runtime.ReadMemStats(&m1)
		if !errors.Is(err, ErrResizeBound) {
			t.Fatalf("size %d: %v", size, err)
		}
		if grew := m1.TotalAlloc - m0.TotalAlloc; grew > 1<<20 {
			t.Fatalf("size %d: refused after allocating %d bytes", size, grew)
		}
	}
	// The cap itself is allowed to be tried (it fits no FAT12 but is a bound, not a type).
	if info, err := s.CopyResized(MaxResize, "fat32", ""); err != nil || info.Size != MaxResize || info.Type != "fat32" {
		t.Fatalf("the largest copy: %+v, %v", info, err)
	}
	s.Close()
	if _, _, err := s.ReadCopy(0, 1); err == nil {
		t.Fatal("Close left the copy")
	}
	for _, bad := range []string{"fat64", "exfat", "FAT32"} {
		s2, _ := openSample(t, DefaultLimits)
		if _, err := s2.CopyResized(32<<20, bad, ""); !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("type %q: %v", bad, err)
		}
	}
	s3, _ := openSample(t, DefaultLimits)
	if _, err := s3.CopyResized(32<<20, "auto", "TWELVE BYTES"); err == nil {
		t.Fatal("a 12 byte label")
	}
}

func TestReadCopyEdges(t *testing.T) {
	s, _ := openSample(t, DefaultLimits)
	if _, _, err := s.ReadCopy(0, 1); err == nil {
		t.Fatal("no copy made, yet read")
	}
	if _, err := s.CopyResized(16<<20, "auto", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadCopy(-1, 1); err == nil {
		t.Fatal("negative offset")
	}
	if _, _, err := s.ReadCopy(0, -1); err == nil {
		t.Fatal("negative length")
	}
	b, size, err := s.ReadCopy(16<<20, 10)
	if err != nil || len(b) != 0 || size != 16<<20 {
		t.Fatalf("past the end: %d bytes of %d, %v", len(b), size, err)
	}
	b, _, err = s.ReadCopy(0, 1<<30)
	if err != nil || int64(len(b)) != DefaultLimits.MaxChunk {
		t.Fatalf("a chunk is capped: %d, %v", len(b), err)
	}
}

// Through the worker's protocol: the request carries the typed size, and the
// answer carries the type and size, or the refusal in words.
func TestResizeProtocol(t *testing.T) {
	s, _ := openSample(t, DefaultLimits)
	resp, _ := s.Handle([]byte(`{"op":"resize","size":33554432,"type":"fat16","label":"PLAY"}`))
	if !bytes.Contains(resp, []byte(`"ok":true`)) || !bytes.Contains(resp, []byte(`"type":"fat16"`)) || !bytes.Contains(resp, []byte(`"size":33554432`)) {
		t.Fatalf("resize: %s", resp)
	}
	if again := openBytes(t, fetchCopy(t, s)); again.Info().Type != "fat16" {
		t.Fatalf("downloaded copy is %s", again.Info().Type)
	}
	for _, req := range []string{
		`{"op":"resize","size":99999999999}`,
		`{"op":"resize","size":4194304,"type":"fat32"}`,
		`{"op":"resize"}`,
	} {
		resp, data := s.Handle([]byte(req))
		if bytes.Contains(resp, []byte(`"ok":true`)) || data != nil || !bytes.Contains(resp, []byte(`"error"`)) {
			t.Fatalf("%s: %s", req, resp)
		}
	}
}

// The action against fat32.CopyResized called directly, same source, same
// size: the session's overhead is the copy kept, the detect pass over the result
// and the lock.
func benchSource(b *testing.B) []byte {
	img, err := Sample()
	if err != nil {
		b.Fatal(err)
	}
	return img
}

func BenchmarkCopyResizedSession(b *testing.B) {
	img := benchSource(b)
	s := NewSession(DefaultLimits)
	if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.CopyResized(32<<20, "fat16", ""); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyResizedDirect(b *testing.B) {
	img := benchSource(b)
	src, err := filesystem_fat32.OpenReader(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst := &memImage{b: make([]byte, 32<<20)}
		out, err := filesystem_fat32.CopyResized(dst, 32<<20, src, filesystem_fat32.CopyConfig{Type: filesystem_fat32.FAT16})
		if err != nil {
			b.Fatal(err)
		}
		out.Close()
	}
}
