package core

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"testing"
)

//go:embed testdata/fat12.img.gz testdata/fat16.img.gz
var smallFAT embed.FS

// Small FAT images (what newfs_msdos -F 12|16 and mkfs.vfat make below ~33 MiB)
// open: detected, and forced by the page's FAT12/FAT16 choices.
func TestSmallFATImagesOpen(t *testing.T) {
	for name, kind := range map[string]string{"fat12.img.gz": "fat12", "fat16.img.gz": "fat16"} {
		f, err := smallFAT.Open("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		img, err := io.ReadAll(zr)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"auto", kind} {
			s := NewSession(DefaultLimits)
			info, err := s.OpenAs(bytes.NewReader(img), int64(len(img)), k)
			if err != nil || info.Type != kind {
				t.Fatalf("%s as %s: %+v, %v", name, k, info, err)
			}
			if _, err := s.List("/"); err != nil {
				t.Fatalf("%s as %s: List: %v", name, k, err)
			}
		}
	}
}
