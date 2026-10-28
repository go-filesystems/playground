package core

import (
	"container/list"
	"errors"
	"io"
	"sync"
)

// Counter is an io.ReaderAt that counts what reaches the reader under it:
// in the browser every call is one FileReaderSync round trip, so the count
// is the cost the block cache exists to cut.
type Counter struct {
	r     io.ReaderAt
	mu    sync.Mutex
	calls int64
	bytes int64
}

// NewCounter wraps r.
func NewCounter(r io.ReaderAt) *Counter { return &Counter{r: r} }

// ReadAt reads from the wrapped reader and counts the call.
func (c *Counter) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.mu.Lock()
	c.calls++
	c.bytes += int64(n)
	c.mu.Unlock()
	return n, err
}

// Counts returns the calls and bytes so far.
func (c *Counter) Counts() (calls, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.bytes
}

// BlockCache is an io.ReaderAt that reads the reader under it in aligned
// blocks and keeps the most recently used ones (LRU). Filesystem drivers
// issue many small reads (a 512-byte sector, a 32-byte directory entry, a
// 4 KiB inode block), most of them close together; one block read answers
// all of them.
//
// Reads of at least bypassBlocks blocks go straight to the reader and are not
// cached: they are file contents being streamed, which would only evict the
// metadata the cache is for.
type BlockCache struct {
	r         io.ReaderAt
	size      int64
	blockSize int64
	maxBlocks int

	mu     sync.Mutex
	blocks map[int64]*list.Element
	lru    *list.List // front = most recently used; values are *block
	hits   int64
	misses int64
}

type block struct {
	idx  int64
	data []byte
}

const bypassBlocks = 4

// NewBlockCache caches r, which holds size bytes, in blocks of blockSize
// bytes, keeping at most maxBlocks of them.
func NewBlockCache(r io.ReaderAt, size int64, blockSize, maxBlocks int) *BlockCache {
	if blockSize <= 0 {
		blockSize = 64 << 10
	}
	if maxBlocks <= 0 {
		maxBlocks = 1
	}
	return &BlockCache{r: r, size: size, blockSize: int64(blockSize), maxBlocks: maxBlocks,
		blocks: map[int64]*list.Element{}, lru: list.New()}
}

// ErrNegativeOffset is returned for a read before the start of the image.
var ErrNegativeOffset = errors.New("core: negative offset")

// ReadAt follows io.ReaderAt: a short read comes with an error, io.EOF when
// the image ends.
func (c *BlockCache) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if off >= c.size {
		return 0, io.EOF
	}
	want := p
	if rest := c.size - off; int64(len(want)) > rest {
		want = want[:rest]
	}
	if int64(len(want)) >= bypassBlocks*c.blockSize {
		n, err := c.r.ReadAt(want, off)
		if err == io.EOF && n == len(want) {
			err = nil
		}
		if err == nil && n < len(p) {
			err = io.EOF
		}
		return n, err
	}
	n := 0
	for n < len(want) {
		idx := (off + int64(n)) / c.blockSize
		b, err := c.get(idx)
		if err != nil {
			return n, err
		}
		// get returns whole blocks (a short one is an error), and want
		// ends before size, so within is always inside b.
		within := off + int64(n) - idx*c.blockSize
		n += copy(want[n:], b[within:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// get returns block idx, reading it on a miss.
func (c *BlockCache) get(idx int64) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.blocks[idx]; ok {
		c.lru.MoveToFront(e)
		c.hits++
		data := e.Value.(*block).data
		c.mu.Unlock()
		return data, nil
	}
	c.misses++
	c.mu.Unlock()

	start := idx * c.blockSize
	n := min(c.blockSize, c.size-start)
	buf := make([]byte, n)
	got, err := c.r.ReadAt(buf, start)
	if err != nil && !(err == io.EOF && int64(got) == n) {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.blocks[idx]; ok { // filled meanwhile by another reader
		c.lru.MoveToFront(e)
		return e.Value.(*block).data, nil
	}
	c.blocks[idx] = c.lru.PushFront(&block{idx: idx, data: buf})
	for c.lru.Len() > c.maxBlocks {
		last := c.lru.Back()
		c.lru.Remove(last)
		delete(c.blocks, last.Value.(*block).idx)
	}
	return buf, nil
}

// Stats returns the cache's hits and misses and how many blocks it holds.
func (c *BlockCache) Stats() (hits, misses int64, blocks int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.lru.Len()
}
