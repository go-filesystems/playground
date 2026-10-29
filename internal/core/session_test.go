package core

import (
	"bytes"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	filesystem_exfat "github.com/go-filesystems/exfat"
	filesystem_ext4 "github.com/go-filesystems/ext4"
	filesystem "github.com/go-filesystems/interface"
)

func openSample(t *testing.T, lim Limits) (*Session, []byte) {
	t.Helper()
	img, err := Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	s := NewSession(lim)
	info, err := s.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if info.Type != "fat32" || info.Label != "PLAYGROUND" || !info.Partial || info.Size != SampleSize {
		t.Fatalf("info = %+v", info)
	}
	return s, img
}

func names(l Listing) string {
	var n []string
	for _, e := range l.Entries {
		n = append(n, e.Name+":"+e.Kind)
	}
	return strings.Join(n, ",")
}

func TestSampleBrowse(t *testing.T) {
	for _, cached := range []bool{true, false} {
		lim := DefaultLimits
		if !cached {
			lim.MaxBlocks = 0
		}
		s, _ := openSample(t, lim)
		if s.Info().Cached != cached {
			t.Fatalf("cached = %v", s.Info().Cached)
		}
		root, err := s.List("/")
		if err != nil {
			t.Fatal(err)
		}
		if got := names(root); got != "docs:dir,empty:dir,images:dir,README.TXT:file" {
			t.Fatalf("root = %s", got)
		}
		docs, err := s.List("/docs/")
		if err != nil || docs.Path != "/docs" || len(docs.Entries) != 3 {
			t.Fatalf("docs = %+v, %v", docs, err)
		}
		files, _ := sampleFiles()
		got, size, err := s.Read("/docs/hello.txt", 0, 1<<20)
		if err != nil || !bytes.Equal(got, files["/docs/hello.txt"]) || size != int64(len(got)) {
			t.Fatalf("read = %q %d %v", got, size, err)
		}
		// Ranges of a larger file, as the page streams them.
		lines := files["/docs/lines.txt"]
		var all []byte
		for off := int64(0); ; {
			b, size, err := s.Read("/docs/lines.txt", off, 10000)
			if err != nil {
				t.Fatal(err)
			}
			if size != int64(len(lines)) {
				t.Fatalf("size %d", size)
			}
			if len(b) == 0 {
				break
			}
			all = append(all, b...)
			off += int64(len(b))
		}
		if !bytes.Equal(all, lines) {
			t.Fatal("chunked read differs")
		}
		st := s.Stats()
		if st.BackendReads == 0 || (cached && st.CacheMisses == 0) {
			t.Fatalf("stats %+v", st)
		}
		s.Close()
		if _, err := s.List("/"); !errors.Is(err, ErrNotOpen) {
			t.Fatalf("after close: %v", err)
		}
	}
}

func TestReadEdges(t *testing.T) {
	lim := DefaultLimits
	lim.MaxChunk = 7
	s, _ := openSample(t, lim)
	if b, _, err := s.Read("/README.TXT", 0, 1000); err != nil || len(b) != 7 {
		t.Fatalf("chunk cap: %d %v", len(b), err)
	}
	if b, size, err := s.Read("/README.TXT", 1<<40, 10); err != nil || len(b) != 0 || size == 0 {
		t.Fatalf("past end: %d %d %v", len(b), size, err)
	}
	if _, _, err := s.Read("/docs", 0, 10); !errors.Is(err, ErrIsDir) {
		t.Fatalf("dir: %v", err)
	}
	if _, _, err := s.Read("/nope", 0, 10); !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := s.Read("/README.TXT", -1, 10); err == nil {
		t.Fatal("negative offset accepted")
	}
	for _, p := range []string{"", "rel", "/a\x00b", "/" + strings.Repeat("a", 5000)} {
		if _, _, err := s.Read(p, 0, 1); !errors.Is(err, ErrBadPath) {
			t.Fatalf("path %q: %v", p, err)
		}
		if _, err := s.List(p); !errors.Is(err, ErrBadPath) {
			t.Fatalf("list %q: %v", p, err)
		}
	}
	if _, err := s.List("/missing"); err == nil {
		t.Fatal("listing a missing dir succeeded")
	}
	empty := NewSession(DefaultLimits)
	if _, _, err := empty.Read("/x", 0, 1); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("not open: %v", err)
	}
}

func TestListLimits(t *testing.T) {
	lim := DefaultLimits
	lim.MaxEntries = 2
	lim.MaxStat = 0 // kinds from the directory entry alone
	s, _ := openSample(t, lim)
	l, err := s.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Truncated || len(l.Entries) != 2 || l.Total != 4 {
		t.Fatalf("listing %+v", l)
	}
	for _, e := range l.Entries {
		if e.Size != 0 {
			t.Fatalf("unstatted entry has a size: %+v", e)
		}
	}
}

func TestOpenRefusals(t *testing.T) {
	s := NewSession(DefaultLimits)
	if _, err := s.Open(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("empty image opened")
	}
	info, err := s.Open(bytes.NewReader(make([]byte, 1<<20)), 1<<20)
	if err == nil || info.Type != "unknown" {
		t.Fatalf("zeros: %+v %v", info, err)
	}
	// Recognised, then broken: a FAT32 boot sector with nothing behind it.
	img, _ := Sample()
	bad := append([]byte(nil), img[:512]...)
	bad = append(bad, make([]byte, 4096)...)
	// fat32 opens lazily from the boot sector, so the damage shows at the
	// first listing -- as an error, not a panic or a hang.
	info, err = s.Open(bytes.NewReader(bad), int64(len(bad)))
	if info.Type != "fat32" {
		t.Fatalf("truncated fat32 not recognised: %+v %v", info, err)
	}
	if err == nil {
		if _, err := s.List("/"); err == nil {
			t.Fatal("a FAT32 image cut after its boot sector listed cleanly")
		}
	}
	s.Close()
	resp, _ := s.Handle([]byte(`{"op":"info"}`))
	if !bytes.Contains(resp, []byte(`"ok":false`)) {
		t.Fatalf("info with nothing open: %s", resp)
	}
}

// plainFS hides every optional capability: a driver without Opener.
type plainFS struct{ filesystem.Filesystem }

func TestReadWholeFileDriver(t *testing.T) {
	lim := DefaultLimits
	lim.MaxWhole = 50000
	lim.MaxChunk = 1000
	s, _ := openSample(t, lim)
	s.fs = plainFS{s.fs}
	files, _ := sampleFiles()
	lines := files["/docs/lines.txt"]
	if len(lines) <= 50000 {
		// lines.txt must exceed MaxWhole for the refusal below.
		t.Fatalf("lines.txt is only %d bytes", len(lines))
	}
	if _, _, err := s.Read("/docs/lines.txt", 0, 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("whole-file limit: %v", err)
	}
	readme := files["/README.TXT"]
	var all []byte
	for off := int64(0); ; {
		b, _, err := s.Read("/README.TXT", off, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			break
		}
		all = append(all, b...)
		off += int64(len(b))
	}
	if !bytes.Equal(all, readme) {
		t.Fatal("whole-file chunks differ")
	}
	if s.wholePath != "/README.TXT" {
		t.Fatal("the whole file was not kept for the next chunk")
	}
	// The file shrank between Stat and ReadFile (a racing or lying driver).
	s.whole = s.whole[:3]
	if b, _, err := s.Read("/README.TXT", 10, 5); err != nil || len(b) != 0 {
		t.Fatalf("shrunk: %q %v", b, err)
	}
	if _, _, err := s.Read("/nope", 0, 5); err == nil {
		t.Fatal("missing file read")
	}
	s.fs = brokenFS{plainFS{s.fs}}
	s.wholePath, s.whole = "", nil
	if _, _, err := s.Read("/README.TXT", 0, 5); err == nil {
		t.Fatal("ReadFile error swallowed")
	}
}

// brokenFS fails ReadFile and panics on ListDir: a driver meeting a
// structure it did not expect.
type brokenFS struct{ plainFS }

func (brokenFS) ReadFile(string) ([]byte, error)               { return nil, errors.New("broken") }
func (brokenFS) ListDir(string) ([]filesystem.DirEntry, error) { panic("index out of range") }

func TestDriverPanicIsAnError(t *testing.T) {
	s, _ := openSample(t, DefaultLimits)
	s.fs = brokenFS{plainFS{s.fs}}
	if _, err := s.List("/"); err == nil || !strings.Contains(err.Error(), "driver failed") {
		t.Fatalf("panic not turned into an error: %v", err)
	}
	if got := safeCall(func() string { panic("x") }); got != "" {
		t.Fatalf("safeCall = %q", got)
	}
	for _, k := range Kinds {
		if _, _, err := openGuarded(panicReader{}, 1<<20, k); err == nil {
			t.Fatalf("a panicking reader opened as %s", k)
		}
	}
}

// Every kind the page offers is one OpenAs takes, the page's list is
// exactly Kinds, and a kind nobody offers is refused.
func TestFilesystemKinds(t *testing.T) {
	html, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	var offered []string
	for _, m := range regexp.MustCompile(`<option value="([a-z0-9]+)"`).FindAllStringSubmatch(string(html), -1) {
		offered = append(offered, m[1])
	}
	if strings.Join(offered, ",") != strings.Join(Kinds, ",") {
		t.Fatalf("the page offers %v, the engine takes %v", offered, Kinds)
	}
	s := NewSession(DefaultLimits)
	img, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAs(bytes.NewReader(img), int64(len(img)), "nope"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("unknown kind: %v", err)
	}
	// The sample is FAT32: forced as FAT32 it opens and says so; forced as
	// ext4 it does not; auto detects it.
	info, err := s.OpenAs(bytes.NewReader(img), int64(len(img)), "fat32")
	if err != nil || info.Type != "fat32" || !info.Forced {
		t.Fatalf("forced fat32: %+v %v", info, err)
	}
	for _, k := range []string{"ext4", "apfs", "btrfs", "xfs", "zfs"} {
		if _, err := s.OpenAs(bytes.NewReader(img), int64(len(img)), k); err == nil {
			t.Fatalf("a FAT32 image opened as %s", k)
		}
	}
	info, err = s.OpenAs(bytes.NewReader(img), int64(len(img)), "auto")
	if err != nil || info.Type != "fat32" || info.Forced {
		t.Fatalf("auto: %+v %v", info, err)
	}
	// Nothing detectable: every device driver is tried, and the error is
	// detect's.
	if _, err := s.OpenAs(bytes.NewReader(make([]byte, 1<<20)), 1<<20, "auto"); err == nil {
		t.Fatal("zeros opened")
	}
	// The read-only device refuses writes.
	d := roDevice{r: bytes.NewReader(img), size: int64(len(img))}
	if _, err := d.WriteAt([]byte{1}, 0); !errors.Is(err, os.ErrPermission) || d.Truncate(0) == nil || d.Sync() != nil || d.Close() != nil {
		t.Fatal("roDevice")
	}
	if n, _ := d.Size(); n != int64(len(img)) {
		t.Fatal("roDevice size")
	}
}

type panicReader struct{}

func (panicReader) ReadAt([]byte, int64) (int, error) { panic("reader exploded") }

// openerFailFS has OpenFile fail and a File whose ReadAt fails.
type openerFailFS struct {
	filesystem.Filesystem
	openErr error
}

func (o openerFailFS) OpenFile(string) (filesystem.File, error) {
	if o.openErr != nil {
		return nil, o.openErr
	}
	return badFile{}, nil
}

type badFile struct{}

func (badFile) ReadAt(p []byte, off int64) (int, error) {
	if off == 0 {
		return 0, errors.New("io error")
	}
	return 1, io.EOF
}
func (badFile) Close() error { return nil }
func (badFile) Size() int64  { return 1 << 20 }

func TestOpenerFailures(t *testing.T) {
	s, _ := openSample(t, DefaultLimits)
	inner := s.fs
	s.fs = openerFailFS{Filesystem: inner, openErr: errors.New("no open")}
	if _, _, err := s.Read("/docs/lines.txt", 0, 5); err == nil {
		t.Fatal("OpenFile error swallowed")
	}
	s.fs = openerFailFS{Filesystem: inner}
	if _, _, err := s.Read("/docs/lines.txt", 0, 5); err == nil {
		t.Fatal("ReadAt error swallowed")
	}
	// A short read with io.EOF before the size: what was read is returned.
	if b, _, err := s.Read("/docs/lines.txt", 10, 5); err != nil || len(b) != 1 {
		t.Fatalf("short EOF read: %d %v", len(b), err)
	}
}

func TestKinds(t *testing.T) {
	for mode, want := range map[uint16]string{0o040755: "dir", 0o100644: "file", 0o120777: "link", 0o060600: "other"} {
		if got := kindOf(mode); got != want {
			t.Fatalf("kindOf(%o) = %s", mode, got)
		}
	}
	for ft, want := range map[uint8]string{1: "file", 2: "dir", 7: "link", 3: "other"} {
		if got := kindOfType(ft); got != want {
			t.Fatalf("kindOfType(%d) = %s", ft, got)
		}
	}
}

// TestOtherFilesystems formats exFAT and ext4 images with their drivers and
// opens them through the session: detection beyond FAT32.
func TestOtherFilesystems(t *testing.T) {
	type mk func(path string) (filesystem.Filesystem, error)
	cases := map[string]mk{
		"exfat": func(p string) (filesystem.Filesystem, error) {
			return filesystem_exfat.Format(p, 16<<20, filesystem_exfat.FormatConfig{})
		},
		"ext4": func(p string) (filesystem.Filesystem, error) {
			return filesystem_ext4.Format(p, 16<<20, filesystem_ext4.FormatConfig{})
		},
	}
	for typ, format := range cases {
		t.Run(typ, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), typ+".img")
			fsys, err := format(p)
			if err != nil {
				t.Fatalf("format: %v", err)
			}
			if err := fsys.WriteFile("/hello.txt", []byte("hi "+typ), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			fsys.Close()
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, _ := f.Stat()
			s := NewSession(DefaultLimits)
			info, err := s.Open(f, st.Size())
			if err != nil || info.Type != typ {
				t.Fatalf("open: %+v %v", info, err)
			}
			b, _, err := s.Read("/hello.txt", 0, 100)
			if err != nil || string(b) != "hi "+typ {
				t.Fatalf("read: %q %v", b, err)
			}
		})
	}
}

func TestSetCache(t *testing.T) {
	s := NewSession(Limits{MaxEntries: 10, MaxChunk: 10, MaxWhole: 10})
	s.SetCache(true)
	if s.lim.MaxBlocks != DefaultLimits.MaxBlocks {
		t.Fatal("cache not turned on")
	}
	s.SetCache(true) // already on: unchanged
	s.SetCache(false)
	if s.lim.MaxBlocks != 0 {
		t.Fatal("cache not turned off")
	}
}

func TestMemImage(t *testing.T) {
	m := &memImage{b: make([]byte, 10)}
	if _, err := m.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative read")
	}
	if n, err := m.ReadAt(make([]byte, 4), 8); n != 2 || err != io.EOF {
		t.Fatalf("tail read %d %v", n, err)
	}
	if _, err := m.ReadAt(make([]byte, 1), 10); err != io.EOF {
		t.Fatal("read at end")
	}
	if _, err := m.WriteAt(make([]byte, 4), 8); err == nil {
		t.Fatal("write past end")
	}
}
