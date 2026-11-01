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

// The explorer's changes on a small FAT image: with fat32 v0.7.0 FAT12 and
// FAT16 take writes through the copy-on-write layer like FAT32, and the
// modified image downloads and reopens with the change in it.
func TestSmallFATImagesAreWritable(t *testing.T) {
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
		s := NewSession(DefaultLimits)
		if info, err := s.Open(bytes.NewReader(img), int64(len(img))); err != nil || info.Type != kind {
			t.Fatalf("%s: %+v, %v", name, info, err)
		}
		if a, err := s.DAV("MKCOL", "/MANY/made", nil, nil); err != nil || a.Status >= 300 {
			t.Fatalf("%s MKCOL: %d, %v", name, a.Status, err)
		}
		if a, err := s.DAV("PUT", "/MANY/made/hello.txt", nil, []byte("hello from the tab")); err != nil || a.Status >= 300 {
			t.Fatalf("%s PUT: %d, %v", name, a.Status, err)
		}
		out := downloadImage(t, s)
		again := NewSession(DefaultLimits)
		if info, err := again.Open(bytes.NewReader(out), int64(len(out))); err != nil || info.Type != kind {
			t.Fatalf("%s reopening: %+v, %v", name, info, err)
		}
		if got, _, err := again.Read("/MANY/made/hello.txt", 0, 100); err != nil || string(got) != "hello from the tab" {
			t.Fatalf("%s the written file: %q, %v", name, got, err)
		}
	}
}
