package core

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/go-filesystems/detect"
	filesystem_fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
)

// MaxResize bounds the image CopyResized makes: 256 MiB, the same memory the
// page gives a change set (MaxChanges). The size comes from a typed field, so
// it is checked before anything is allocated.
const MaxResize = MaxChanges

// ResizeSector is the unit a FAT image's size is a multiple of.
const ResizeSector = 512

// Errors CopyResized returns beside fat32's own (ErrCopyNoFit, ErrCopyRootFull).
var (
	ErrNotFAT      = errors.New("only a FAT image can be copied to a resized image")
	ErrResizeBound = errors.New("the size is outside what the page can build")
)

// resizeTypes are the FAT types a person can ask for; "auto" lets the size decide.
var resizeTypes = map[string]filesystem_fat32.FATType{
	"":      0,
	"auto":  0,
	"fat12": filesystem_fat32.FAT12,
	"fat16": filesystem_fat32.FAT16,
	"fat32": filesystem_fat32.FAT32,
}

// CopyResized formats a new FAT image of size bytes in memory (kind: "auto",
// "fat12", "fat16" or "fat32"; label, when not empty, is the new volume's) and
// copies the open FAT image's whole tree into it, with fat32's CopyResized. It
// is how a FAT image grows here, and changes type: an image in this tab cannot
// be truncated. The open image, changes included, is only read.
//
// The copy is kept in the session, replacing the previous one, until the next
// Open or Close; ReadCopy reads it. The returned Info describes it.
func (s *Session) CopyResized(size int64, kind, label string) (info Info, err error) {
	if size <= 0 || size > MaxResize || size%ResizeSector != 0 {
		return Info{}, fmt.Errorf("%w: %d bytes (a positive multiple of %d, at most %d MiB)", ErrResizeBound, size, ResizeSector, MaxResize>>20)
	}
	typ, ok := resizeTypes[kind]
	if !ok {
		return Info{}, fmt.Errorf("%w: %q (auto, fat12, fat16 or fat32)", ErrUnknownKind, kind)
	}
	if len(label) > 11 {
		return Info{}, fmt.Errorf("the label %q is longer than 11 bytes", label)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fs == nil {
		return Info{}, ErrNotOpen
	}
	switch s.info.Type {
	case string(detect.FAT12), string(detect.FAT16), string(detect.FAT32):
	default:
		return Info{}, fmt.Errorf("%w (this is %s)", ErrNotFAT, s.info.Type)
	}
	defer guard(&err)
	s.copyImg = nil // the previous copy's memory goes before the next is made
	dst := &memImage{b: make([]byte, size)}
	out, err := filesystem_fat32.CopyResized(dst, size, s.fs, filesystem_fat32.CopyConfig{Type: typ, Label: label})
	if err != nil {
		return Info{}, err
	}
	out.Close()
	// What a reader makes of the result is what the page reports.
	fsys, t, err := detect.Open(bytes.NewReader(dst.b), size)
	if err != nil {
		return Info{}, fmt.Errorf("the copy does not open: %w", err)
	}
	defer fsys.Close()
	info = Info{Type: string(t), Size: size}
	if l, ok := fsys.(filesystem.LabelReader); ok {
		info.Label = safeCall(l.Label)
	}
	s.copyImg = dst.b
	return info, nil
}

// ReadCopy returns at most n bytes (capped at MaxChunk) of the copy made by
// CopyResized, from offset off, and the copy's size.
func (s *Session) ReadCopy(off, n int64) ([]byte, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.copyImg == nil {
		return nil, 0, errors.New("no resized copy was made")
	}
	size := int64(len(s.copyImg))
	if off < 0 || n < 0 {
		return nil, size, fmt.Errorf("negative offset or length")
	}
	if off >= size {
		return []byte{}, size, nil
	}
	return append([]byte(nil), s.copyImg[off:off+min(n, s.lim.MaxChunk, size-off)]...), size, nil
}
