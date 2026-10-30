package core

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// COW makes the dropped image writable without touching it. The browser's
// File can only be read, so a write lands in memory, a block at a time, and a
// read takes each block from there when it was written and from the image
// otherwise. Reading a COW from start to end gives the modified image: the
// same format, every change in it. The image never grows: a driver writes
// inside it, as on a disk.
type COW struct {
	base io.ReaderAt
	size int64
	max  int // the most blocks written

	mu    sync.Mutex
	dirty map[int64][]byte // block index -> its bytes, cowBlock long (the last one shorter)
}

// cowBlock is the unit of a change: 64 KiB, a cache block.
const cowBlock = 64 << 10

// MaxChanges bounds the memory changes may take: 256 MiB.
const MaxChanges = 256 << 20

// ErrNoRoom is a write past MaxChanges of changes.
var ErrNoRoom = errors.New("the changes outgrow the memory the page gives them (256 MiB)")

// ErrPastEnd is a write past the end of the image, which does not grow.
var ErrPastEnd = errors.New("a write past the end of the image")

// NewCOW makes base, of size bytes, writable in memory, with at most
// maxBytes of changes.
func NewCOW(base io.ReaderAt, size int64, maxBytes int) *COW {
	return &COW{base: base, size: size, max: max(1, maxBytes/cowBlock), dirty: map[int64][]byte{}}
}

// Size is the image's size.
func (c *COW) Size() int64 { return c.size }

// Changed is how many bytes of blocks were written.
func (c *COW) Changed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, b := range c.dirty {
		n += int64(len(b))
	}
	return n
}

// ReadAt follows io.ReaderAt.
func (c *COW) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if off >= c.size {
		return 0, io.EOF
	}
	want := p[:min(int64(len(p)), c.size-off)]
	n := 0
	for n < len(want) {
		pos := off + int64(n)
		idx := pos / cowBlock
		within := pos - idx*cowBlock
		span := min(int64(len(want)-n), cowBlock-within)
		c.mu.Lock()
		b, ok := c.dirty[idx]
		if ok {
			copy(want[n:n+int(span)], b[within:])
		}
		c.mu.Unlock()
		if !ok {
			// A run of untouched blocks is one read of the image.
			for end := idx + 1; int64(n)+span < int64(len(want)); end++ {
				c.mu.Lock()
				_, d := c.dirty[end]
				c.mu.Unlock()
				if d {
					break
				}
				span = min(int64(len(want)-n), (end+1)*cowBlock-pos)
			}
			got, err := c.base.ReadAt(want[n:n+int(span)], pos)
			if got < int(span) {
				if err == nil || err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return n + got, err
			}
		}
		n += int(span)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt writes p at off, into memory.
func (c *COW) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if off > c.size || int64(len(p)) > c.size-off {
		return 0, ErrPastEnd
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Room first, for every block this write touches, so a write either
	// lands whole or not at all.
	first, last := off/cowBlock, (off+int64(len(p))-1)/cowBlock
	need := 0
	for i := first; i <= last && len(p) > 0; i++ {
		if _, ok := c.dirty[i]; !ok {
			need++
		}
	}
	if len(c.dirty)+need > c.max {
		return 0, ErrNoRoom
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		idx := pos / cowBlock
		b, ok := c.dirty[idx]
		if !ok {
			start := idx * cowBlock
			b = make([]byte, min(cowBlock, c.size-start))
			if got, err := c.base.ReadAt(b, start); got < len(b) {
				if err == nil || err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return n, fmt.Errorf("reading the block to change: %w", err)
			}
			c.dirty[idx] = b
		}
		n += copy(b[pos-idx*cowBlock:], p[n:])
	}
	return n, nil
}
