package core

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// TestBlockCacheMatchesReader: every read through the cache equals the same
// read straight from the bytes, including its error, for random ranges.
func TestBlockCacheMatchesReader(t *testing.T) {
	data := pattern(1<<20 + 123)
	ref := bytes.NewReader(data)
	c := NewBlockCache(NewCounter(bytes.NewReader(data)), int64(len(data)), 4096, 8)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		off := rng.Int63n(int64(len(data)) + 100)
		n := rng.Intn(40000)
		if i%50 == 0 {
			n = rng.Intn(4096*6) + 4096*4 // the bypass path
		}
		a, b := make([]byte, n), make([]byte, n)
		na, ea := ref.ReadAt(a, off)
		nb, eb := c.ReadAt(b, off)
		if na != nb || !bytes.Equal(a[:na], b[:nb]) || (ea == nil) != (eb == nil) {
			t.Fatalf("read %d@%d: ref (%d,%v) cache (%d,%v)", n, off, na, ea, nb, eb)
		}
	}
	hits, misses, blocks := c.Stats()
	if hits == 0 || misses == 0 || blocks > 8 {
		t.Fatalf("hits %d misses %d blocks %d", hits, misses, blocks)
	}
}

func TestBlockCacheCutsBackendReads(t *testing.T) {
	data := pattern(256 << 10)
	cnt := NewCounter(bytes.NewReader(data))
	c := NewBlockCache(cnt, int64(len(data)), 64<<10, 4)
	buf := make([]byte, 512)
	for off := int64(0); off < 64<<10; off += 512 {
		if _, err := c.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if calls, _ := cnt.Counts(); calls != 1 {
		t.Fatalf("128 sector reads inside one block cost %d backend reads, want 1", calls)
	}
}

func TestBlockCacheEdges(t *testing.T) {
	data := pattern(10000)
	c := NewBlockCache(bytes.NewReader(data), int64(len(data)), 0, 0) // defaults
	if _, err := c.ReadAt(make([]byte, 1), -1); !errors.Is(err, ErrNegativeOffset) {
		t.Fatalf("negative offset: %v", err)
	}
	if n, err := c.ReadAt(make([]byte, 1), 10000); n != 0 || err != io.EOF {
		t.Fatalf("at end: %d %v", n, err)
	}
	if n, err := c.ReadAt(make([]byte, 0), 5); n != 0 || err != nil {
		t.Fatalf("empty read: %d %v", n, err)
	}
	// Eviction with one block: reading two blocks alternately.
	c1 := NewBlockCache(bytes.NewReader(data), int64(len(data)), 1024, 1)
	for i := 0; i < 3; i++ {
		for _, off := range []int64{0, 5000} {
			b := make([]byte, 10)
			if _, err := c1.ReadAt(b, off); err != nil || !bytes.Equal(b, data[off:off+10]) {
				t.Fatalf("evicting read at %d: %v", off, err)
			}
		}
	}
	if _, _, blocks := c1.Stats(); blocks != 1 {
		t.Fatalf("blocks = %d, want 1", blocks)
	}
	// A bypass read that runs into the end.
	c2 := NewBlockCache(bytes.NewReader(data), int64(len(data)), 1024, 4)
	big := make([]byte, 8000)
	if n, err := c2.ReadAt(big, 4000); n != 6000 || err != io.EOF {
		t.Fatalf("bypass at end: %d %v", n, err)
	}
	if n, err := c2.ReadAt(big, 2000); n != 8000 || err != nil {
		t.Fatalf("bypass exact end: %d %v", n, err)
	}
}

type failingReader struct{ err error }

func (f failingReader) ReadAt(p []byte, off int64) (int, error) { return 0, f.err }

// shortReader claims more than it holds.
type shortReader struct{ b []byte }

func (s shortReader) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(s.b).ReadAt(p, off)
}

func TestBlockCacheBackendFailures(t *testing.T) {
	boom := errors.New("boom")
	c := NewBlockCache(failingReader{boom}, 1<<20, 4096, 4)
	if _, err := c.ReadAt(make([]byte, 10), 0); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	// The reader is shorter than the size it was given.
	s := NewBlockCache(shortReader{pattern(100)}, 10000, 4096, 4)
	if _, err := s.ReadAt(make([]byte, 10), 50); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short backend: %v", err)
	}
	// A block read that returns everything with io.EOF is fine.
	e := NewBlockCache(bytes.NewReader(pattern(100)), 100, 4096, 4)
	if n, err := e.ReadAt(make([]byte, 100), 0); n != 100 || err != nil {
		t.Fatalf("EOF-with-all: %d %v", n, err)
	}
}

// TestBlockCacheShortBlock: the reader under the cache returns a block that
// is shorter than claimed without error -- a broken io.ReaderAt -- and the
// cache must not loop or slice out of range.
type liarReader struct{}

func (liarReader) ReadAt(p []byte, off int64) (int, error) { return 0, io.EOF }

func TestBlockCacheLiar(t *testing.T) {
	c := NewBlockCache(liarReader{}, 1<<20, 4096, 4)
	if _, err := c.ReadAt(make([]byte, 10), 100); err == nil {
		t.Fatal("a reader that returns nothing was accepted")
	}
}

func TestBlockCacheRace(t *testing.T) {
	data := pattern(1 << 18)
	c := NewBlockCache(bytes.NewReader(data), int64(len(data)), 4096, 16)
	done := make(chan bool)
	for g := 0; g < 8; g++ {
		go func(g int) {
			rng := rand.New(rand.NewSource(int64(g)))
			b := make([]byte, 700)
			for i := 0; i < 500; i++ {
				off := rng.Int63n(int64(len(data) - 700))
				if _, err := c.ReadAt(b, off); err != nil || !bytes.Equal(b, data[off:off+700]) {
					t.Errorf("concurrent read at %d: %v", off, err)
				}
			}
			done <- true
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
