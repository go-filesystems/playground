package core

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apfs "github.com/go-filesystems/apfs"
	btrfs "github.com/go-filesystems/btrfs"
	filesystem "github.com/go-filesystems/interface"
	xfs "github.com/go-filesystems/xfs"
	zfs "github.com/go-filesystems/zfs"
)

// TestDeviceKinds formats a real image of each disk-image filesystem with
// its own driver, writes a file into it, and opens it through the
// read-only block device: forced, and found by auto.
func TestDeviceKinds(t *testing.T) {
	if testing.Short() {
		t.Skip("formats four images of 512 MiB (sparse)")
	}
	const size = 512 << 20
	cases := map[string]func(p string) (filesystem.Filesystem, error){
		"apfs":  func(p string) (filesystem.Filesystem, error) { return apfs.Format(p, size, apfs.FormatConfig{}) },
		"btrfs": func(p string) (filesystem.Filesystem, error) { return btrfs.Format(p, size, btrfs.FormatConfig{}) },
		"xfs":   func(p string) (filesystem.Filesystem, error) { return xfs.Format(p, size, xfs.FormatConfig{}) },
		"zfs":   func(p string) (filesystem.Filesystem, error) { return zfs.Format(p, size, zfs.FormatConfig{}) },
	}
	for kind, format := range cases {
		t.Run(kind, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), kind+".img")
			fsys, err := format(p)
			if err != nil {
				t.Fatalf("format: %v", err)
			}
			if err := fsys.WriteFile("/hello.txt", []byte("hello "+kind), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := fsys.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, _ := f.Stat()
			for _, how := range []string{kind, "auto"} {
				s := NewSession(DefaultLimits)
				info, err := s.OpenAs(f, st.Size(), how)
				if err != nil {
					t.Fatalf("open as %s: %v", how, err)
				}
				if info.Type != kind || info.Forced != (how != "auto") {
					t.Fatalf("open as %s: %+v", how, info)
				}
				t.Logf("%s: partial reads %v", kind, info.Partial)
				l, err := s.List("/")
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				var names []string
				for _, e := range l.Entries {
					names = append(names, e.Name)
				}
				if !slices.Contains(names, "hello.txt") {
					t.Fatalf("open as %s: / holds %v", how, names)
				}
				b, _, err := s.Read("/hello.txt", 0, 100)
				if err != nil || string(b) != "hello "+kind {
					t.Fatalf("read: %q %v", b, err)
				}
				s.Close()
			}
		})
	}
}
