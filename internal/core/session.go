// Package core is the playground's engine: it opens an untrusted disk image
// through go-filesystems/detect and answers coarse requests about it -- what
// filesystem is this, list a directory, read part of a file -- with every
// answer bounded.
//
// It is plain Go with no syscall/js, so it is tested natively; cmd/worker
// binds it to a browser Web Worker.
package core

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-filesystems/detect"
	filesystem_exfat "github.com/go-filesystems/exfat"
	filesystem_ext4 "github.com/go-filesystems/ext4"
	filesystem_fat32 "github.com/go-filesystems/fat32"
	"github.com/go-filesystems/hfsplus"
	filesystem "github.com/go-filesystems/interface"
	filesystem_iso9660 "github.com/go-filesystems/iso9660"
	filesystem_ntfs "github.com/go-filesystems/ntfs"
	filesystem_squashfs "github.com/go-filesystems/squashfs"
	filesystem_ufs "github.com/go-filesystems/ufs"
)

var registerOnce sync.Once

// readers are the drivers that open a filesystem at offset zero of an
// io.ReaderAt -- the same eight, the same way, as go-fileshare/fileshare's
// registerDrivers. apfs, btrfs, xfs and zfs open a partition of a disk
// image through a block device instead (devices.go).
var readers = map[detect.Type]detect.Opener{
	detect.FAT32:    filesystem_fat32.OpenReader,
	detect.ExFAT:    filesystem_exfat.OpenReader,
	detect.Ext4:     filesystem_ext4.OpenReader,
	detect.NTFS:     filesystem_ntfs.OpenReader,
	detect.ISO9660:  filesystem_iso9660.OpenReader,
	detect.SquashFS: filesystem_squashfs.OpenReader,
	detect.HFSPlus:  hfsplus.OpenReader,
	detect.UFS:      filesystem_ufs.OpenReader,
}

func registerDrivers() {
	registerOnce.Do(func() {
		for t, o := range readers {
			detect.Register(t, o)
		}
	})
}

// Limits bound what one request can cost. The image is untrusted: a
// directory may claim millions of entries, a file may claim terabytes.
type Limits struct {
	// MaxEntries is the most entries one listing returns.
	MaxEntries int
	// MaxStat is how many entries of a listing are Stat-ed for their size
	// and kind; the rest are reported by the directory entry's type alone.
	MaxStat int
	// MaxChunk is the most bytes one read returns.
	MaxChunk int64
	// MaxWhole is the largest file read from a driver that cannot read part
	// of a file (no filesystem.Opener): such a read loads the whole file.
	MaxWhole int64
	// BlockSize and MaxBlocks size the block cache; MaxBlocks 0 disables it.
	BlockSize int
	MaxBlocks int
}

// DefaultLimits are the browser's: a 16 MiB cache of 64 KiB blocks, 4 MiB
// reads, 10 000 entries per directory.
var DefaultLimits = Limits{
	MaxEntries: 10000,
	MaxStat:    10000,
	MaxChunk:   4 << 20,
	MaxWhole:   64 << 20,
	BlockSize:  64 << 10,
	MaxBlocks:  256,
}

// Errors a caller can tell apart.
var (
	ErrNotOpen  = errors.New("no image is open")
	ErrBadPath  = errors.New("invalid path")
	ErrIsDir    = errors.New("is a directory")
	ErrTooLarge = errors.New("file too large for this driver")
)

// Info describes an opened image.
type Info struct {
	Type    string  `json:"type"`
	Size    int64   `json:"size"`
	Label   string  `json:"label,omitempty"`
	Partial bool    `json:"partialReads"` // the driver reads byte ranges (filesystem.Opener)
	Cached  bool    `json:"cached"`
	OpenMs  float64 `json:"openMs"`
	// Forced is a type the person chose rather than detect's answer.
	Forced bool `json:"forced"`
	// Encryption is the container the filesystem was unlocked from (EncLUKS)
	// or empty. With ErrPassword it is the container that needs a password.
	Encryption string `json:"encryption,omitempty"`
}

// Entry is one directory entry.
type Entry struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // dir, file, link, other
	Size int64  `json:"size"`
}

// Listing is one directory.
type Listing struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Total     int     `json:"total"`
	Truncated bool    `json:"truncated"`
}

// Stats is what reached the image and the cache.
type Stats struct {
	BackendReads int64 `json:"backendReads"`
	BackendBytes int64 `json:"backendBytes"`
	CacheHits    int64 `json:"cacheHits"`
	CacheMisses  int64 `json:"cacheMisses"`
	CacheBlocks  int   `json:"cacheBlocks"`
	// Changed is how many bytes of the image's blocks were written (COW).
	Changed int64 `json:"changedBytes"`
}

// Session is one opened image.
type Session struct {
	mu      sync.Mutex
	lim     Limits
	fs      filesystem.Filesystem
	info    Info
	counter *Counter
	cache   *BlockCache
	dav     http.Handler // the open image's WebDAV, made on first use
	cow     *COW         // the image as changed, in memory

	// The last whole file read from a driver without Opener, so streaming
	// it in chunks does not read it again per chunk.
	wholePath string
	whole     []byte
}

// NewSession returns a session with the given limits.
func NewSession(lim Limits) *Session {
	registerDrivers()
	return &Session{lim: lim}
}

// Open detects and opens the image r of size bytes, closing any image
// opened before.
func (s *Session) Open(r io.ReaderAt, size int64) (Info, error) { return s.OpenAs(r, size, "auto") }

// OpenAs opens the image r as kind, one of Kinds: "auto" detects it (and,
// when detect finds nothing at offset zero, tries the disk-image drivers
// in turn); any other kind forces that driver.
func (s *Session) OpenAs(r io.ReaderAt, size int64, kind string) (Info, error) {
	return s.OpenWith(r, size, kind, nil)
}

// OpenWith is OpenAs for an image that may be encrypted: a LUKS container
// needs password, which an image that is not encrypted ignores. OpenWith
// clears password before it returns.
func (s *Session) OpenWith(r io.ReaderAt, size int64, kind string, password []byte) (Info, error) {
	defer clear(password)
	if !slices.Contains(Kinds, kind) {
		return Info{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	if size <= 0 {
		return Info{}, fmt.Errorf("an image of %d bytes is empty", size)
	}
	t0 := time.Now()
	s.counter = NewCounter(r)
	// Changes go to memory, never to the dropped file (cow.go).
	s.cow = NewCOW(s.counter, size, MaxChanges)
	img, err := unlock(s.cow, size, password)
	clear(password)
	if err != nil {
		return Info{Type: string(detect.Unknown), Size: size, Encryption: img.enc}, err
	}
	var src io.ReaderAt = img.r
	if s.lim.MaxBlocks > 0 {
		// Over the decrypted image, so a cached block is decrypted once.
		s.cache = NewBlockCache(img.r, img.size, s.lim.BlockSize, s.lim.MaxBlocks)
		src = s.cache
	}
	fsys, t, err := openGuarded(src, img.size, kind)
	if err != nil {
		if t != "" && t != detect.Unknown {
			return Info{Type: string(t), Size: size, Encryption: img.enc}, fmt.Errorf("%s: %w", t, err)
		}
		return Info{Type: string(detect.Unknown), Size: size, Encryption: img.enc}, err
	}
	s.fs = fsys
	_, partial := fsys.(filesystem.Opener)
	s.info = Info{Type: string(t), Size: size, Partial: partial, Cached: s.cache != nil, Forced: kind != "auto", Encryption: img.enc}
	if l, ok := fsys.(filesystem.LabelReader); ok {
		s.info.Label = strings.TrimSpace(safeCall(l.Label))
	}
	s.info.OpenMs = msSince(t0)
	return s.info, nil
}

// deviceKinds are tried, in this order, when detect finds nothing.
var deviceKinds = []string{"apfs", "btrfs", "xfs", "zfs"}

// openGuarded opens r as kind with a driver panic turned into an error: the
// image is untrusted and one bad structure must not take the worker down.
func openGuarded(r io.ReaderAt, size int64, kind string) (filesystem.Filesystem, detect.Type, error) {
	switch {
	case kind == "auto":
		fsys, t, err := guarded(func() (filesystem.Filesystem, detect.Type, error) { return detect.Open(r, size) })
		if err == nil {
			return fsys, t, nil
		}
		if slices.Contains(deviceKinds, string(t)) {
			// detect knows the superblock; the driver takes a block device.
			return openGuarded(r, size, string(t))
		}
		if t != "" && t != detect.Unknown {
			return nil, t, err
		}
		for _, k := range deviceKinds {
			if fsys, _, derr := openGuarded(r, size, k); derr == nil {
				return fsys, detect.Type(k), nil
			}
		}
		return nil, t, err
	case readers[detect.Type(kind)] != nil:
		return guarded(func() (filesystem.Filesystem, detect.Type, error) {
			fsys, err := readers[detect.Type(kind)](r, size)
			return fsys, detect.Type(kind), err
		})
	}
	return guarded(func() (filesystem.Filesystem, detect.Type, error) {
		fsys, err := openDevice(kind, r, size)
		return fsys, detect.Type(kind), err
	})
}

func guarded(open func() (filesystem.Filesystem, detect.Type, error)) (fsys filesystem.Filesystem, t detect.Type, err error) {
	defer func() {
		if p := recover(); p != nil {
			fsys, err = nil, fmt.Errorf("driver failed on this image: %v", p)
		}
	}()
	fsys, t, err = open()
	if err != nil {
		fsys = nil // a failed open hands nothing over (and may be a typed nil)
	}
	return fsys, t, err
}

func safeCall(f func() string) (s string) {
	defer func() {
		if recover() != nil {
			s = ""
		}
	}()
	return f()
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

// Close closes the open image, if any.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *Session) closeLocked() {
	if s.fs != nil {
		s.fs.Close()
	}
	s.fs, s.info, s.counter, s.cache, s.whole, s.wholePath, s.dav, s.cow = nil, Info{}, nil, nil, nil, "", nil, nil
}

// cleanPath validates a path from the page: absolute, no NUL, bounded.
func cleanPath(p string) (string, error) {
	if p == "" || p[0] != '/' || len(p) > 4096 || strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("%w: %q", ErrBadPath, p)
	}
	return path.Clean(p), nil
}

const (
	sIFMT  = 0o170000
	sIFDIR = 0o040000
	sIFREG = 0o100000
	sIFLNK = 0o120000
)

func kindOf(mode uint16) string {
	switch uint32(mode) & sIFMT {
	case sIFDIR:
		return "dir"
	case sIFREG:
		return "file"
	case sIFLNK:
		return "link"
	}
	return "other"
}

// kindOfType maps a DirEntry file type (the ext2 d_type numbering the
// drivers share: 1 regular, 2 directory, 7 symlink) when Stat is skipped.
func kindOfType(t uint8) string {
	switch t {
	case 1:
		return "file"
	case 2:
		return "dir"
	case 7:
		return "link"
	}
	return "other"
}

// List lists the directory at p, sorted directories first, at most
// MaxEntries entries.
func (s *Session) List(p string) (l Listing, err error) {
	p, err = cleanPath(p)
	if err != nil {
		return Listing{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fs == nil {
		return Listing{}, ErrNotOpen
	}
	defer guard(&err)
	ents, err := s.fs.ListDir(p)
	if err != nil {
		return Listing{}, err
	}
	l = Listing{Path: p, Total: len(ents), Entries: make([]Entry, 0, min(len(ents), s.lim.MaxEntries))}
	for i, e := range ents {
		if len(l.Entries) >= s.lim.MaxEntries {
			l.Truncated = true
			break
		}
		name := e.Name()
		if name == "." || name == ".." || name == "" {
			l.Total--
			continue
		}
		ent := Entry{Name: name, Kind: kindOfType(e.FileType())}
		if i < s.lim.MaxStat {
			if st, err := s.fs.Stat(path.Join(p, name)); err == nil {
				ent.Kind = kindOf(st.Mode())
				if ent.Kind != "dir" {
					ent.Size = int64(min(st.Size(), 1<<62))
				}
			}
		}
		l.Entries = append(l.Entries, ent)
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if (a.Kind == "dir") != (b.Kind == "dir") {
			return a.Kind == "dir"
		}
		return a.Name < b.Name
	})
	return l, nil
}

// guard turns a driver panic into an error.
func guard(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("driver failed on this image: %v", p)
	}
}

// Read returns at most n bytes (capped at MaxChunk) of the file at p from
// offset off, and the file's size.
func (s *Session) Read(p string, off, n int64) (data []byte, size int64, err error) {
	p, err = cleanPath(p)
	if err != nil {
		return nil, 0, err
	}
	if off < 0 || n < 0 {
		return nil, 0, fmt.Errorf("negative offset or length")
	}
	n = min(n, s.lim.MaxChunk)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fs == nil {
		return nil, 0, ErrNotOpen
	}
	defer guard(&err)
	st, err := s.fs.Stat(p)
	if err != nil {
		return nil, 0, err
	}
	if kindOf(st.Mode()) == "dir" {
		return nil, 0, fmt.Errorf("%s: %w", p, ErrIsDir)
	}
	size = int64(min(st.Size(), 1<<62))
	if off >= size || n == 0 {
		return []byte{}, size, nil
	}
	n = min(n, size-off)
	if o, ok := s.fs.(filesystem.Opener); ok {
		f, err := o.OpenFile(p)
		if err != nil {
			return nil, 0, err
		}
		defer f.Close()
		buf := make([]byte, n)
		got, err := f.ReadAt(buf, off)
		if err != nil && !(err == io.EOF && int64(got) == n) {
			if err == io.EOF {
				return buf[:got], size, nil
			}
			return nil, 0, err
		}
		return buf, size, nil
	}
	if size > s.lim.MaxWhole {
		return nil, size, fmt.Errorf("%s is %d bytes and %s can only read a file whole (limit %d): %w",
			p, size, s.info.Type, s.lim.MaxWhole, ErrTooLarge)
	}
	if s.wholePath != p {
		b, err := s.fs.ReadFile(p)
		if err != nil {
			return nil, 0, err
		}
		s.wholePath, s.whole = p, b
	}
	if off >= int64(len(s.whole)) {
		return []byte{}, size, nil
	}
	end := min(off+n, int64(len(s.whole)))
	return append([]byte(nil), s.whole[off:end]...), size, nil
}

// Stats reports the reads that reached the image and the cache's counters.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st Stats
	if s.counter != nil {
		st.BackendReads, st.BackendBytes = s.counter.Counts()
	}
	if s.cache != nil {
		st.CacheHits, st.CacheMisses, st.CacheBlocks = s.cache.Stats()
	}
	if s.cow != nil {
		st.Changed = s.cow.Changed()
	}
	return st
}

// ReadImage returns at most n bytes (capped at MaxChunk) of the image as
// changed, from offset off, and the image's size: read from start to end, it
// is the modified image to download.
func (s *Session) ReadImage(off, n int64) ([]byte, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cow == nil {
		return nil, 0, ErrNotOpen
	}
	size := s.cow.Size()
	if off < 0 || n < 0 {
		return nil, size, fmt.Errorf("negative offset or length")
	}
	if off >= size {
		return []byte{}, size, nil
	}
	b := make([]byte, min(n, s.lim.MaxChunk, size-off))
	got, err := s.cow.ReadAt(b, off)
	if err == io.EOF && got == len(b) {
		err = nil
	}
	return b[:got], size, err
}

// Info returns the open image's description.
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// SetCache turns the block cache on or off for the next Open.
func (s *Session) SetCache(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !on {
		s.lim.MaxBlocks = 0
	} else if s.lim.MaxBlocks == 0 {
		s.lim.MaxBlocks = DefaultLimits.MaxBlocks
	}
}
