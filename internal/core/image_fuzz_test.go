package core

import (
	"bytes"
	"testing"
)

// FuzzImage damages the metadata of a small FAT32 sample -- the boot
// sector, FSInfo, both FATs and the root directory all live in its first
// 64 KiB -- and browses the result the way the page does. Whatever the bytes,
// the session must answer with an error or a bounded result, never a panic
// that escapes it or a hang (the fuzzer flags an input that runs for 10 s).
func FuzzImage(f *testing.F) {
	base, err := sampleImage(1 << 20)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(uint16(0), []byte{0})
	f.Add(uint16(11), []byte{0, 0})                 // bytes per sector
	f.Add(uint16(13), []byte{0})                    // sectors per cluster
	f.Add(uint16(32*512), []byte{0xff, 0xff, 0xff}) // first FAT
	f.Add(uint16(44), []byte{0xff, 0xff, 0xff, 0x0f})
	f.Fuzz(func(t *testing.T, at uint16, patch []byte) {
		img := append([]byte(nil), base...)
		copy(img[int(at):], patch)
		s := NewSession(DefaultLimits)
		if _, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil {
			return
		}
		defer s.Close()
		var walk func(dir string, depth int)
		walk = func(dir string, depth int) {
			l, err := s.List(dir)
			if err != nil || depth > 3 {
				return
			}
			if len(l.Entries) > DefaultLimits.MaxEntries {
				t.Fatalf("%d entries", len(l.Entries))
			}
			for _, e := range l.Entries[:min(len(l.Entries), 8)] {
				p := dir + "/" + e.Name
				if dir == "/" {
					p = "/" + e.Name
				}
				if e.Kind == "dir" {
					walk(p, depth+1)
					continue
				}
				if b, _, err := s.Read(p, 0, 1<<16); err == nil && len(b) > 1<<16 {
					t.Fatalf("read %d bytes, asked 64 KiB", len(b))
				}
			}
		}
		walk("/", 0)
	})
}
