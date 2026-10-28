package core

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"

	filesystem_fat32 "github.com/go-filesystems/fat32"
)

// SampleSize is the sample image's size: small enough to build in a
// fraction of a second, large enough to be a real FAT32 volume.
const SampleSize = 8 << 20

// memImage is an image in memory that can be read and written.
type memImage struct{ b []byte }

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, errors.New("write outside the image")
	}
	return copy(m.b[off:], p), nil
}

// sampleFiles are the sample volume's contents, by path.
func sampleFiles() (map[string][]byte, error) {
	var logo bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 96; x++ {
			img.Set(x, y, color.RGBA{uint8(40 + x*2), uint8(80 + y), 200, 255})
		}
	}
	if err := png.Encode(&logo, img); err != nil {
		return nil, err
	}
	var big strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&big, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
	}
	return map[string][]byte{
		"/README.TXT": []byte("This FAT32 volume was formatted in your browser by go-filesystems/fat32\n" +
			"(FormatImage), then opened again through go-filesystems/detect.\n" +
			"Nothing left this page.\n"),
		"/docs/hello.txt":      []byte("Hello from a FAT32 image built in WebAssembly.\n"),
		"/docs/notes.md":       []byte("# Notes\n\n- browse the tree\n- view a small file\n- download any file\n"),
		"/docs/lines.txt":      []byte(big.String()),
		"/images/gradient.png": logo.Bytes(),
		"/empty/.keep":         {},
	}, nil
}

// Sample formats a FAT32 volume in memory, writes a few files into it and
// returns the image's bytes.
func Sample() ([]byte, error) { return sampleImage(SampleSize) }

func sampleImage(size int64) ([]byte, error) {
	m := &memImage{b: make([]byte, size)}
	fsys, err := filesystem_fat32.FormatImage(m, size, filesystem_fat32.FormatConfig{Label: "PLAYGROUND"})
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	files, err := sampleFiles()
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"/docs", "/images", "/empty"} {
		if err := fsys.MkDir(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sample: mkdir %s: %w", dir, err)
		}
	}
	for _, p := range []string{"/README.TXT", "/docs/hello.txt", "/docs/notes.md", "/docs/lines.txt", "/images/gradient.png", "/empty/.keep"} {
		if err := fsys.WriteFile(p, files[p], 0o644); err != nil {
			return nil, fmt.Errorf("sample: write %s: %w", p, err)
		}
	}
	return m.b, nil
}
