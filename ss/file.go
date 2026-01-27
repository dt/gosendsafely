package ss

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

type span struct {
	offset int
	length int
}

// Sem is a counting semaphore for limiting concurrent operations.
type Sem chan struct{}

// Limiter creates a semaphore that limits concurrency to n operations.
func Limiter(n int) Sem {
	return make(Sem, n)
}

func (l Sem) Acquire(ctx context.Context) {
	if l != nil {
		select {
		case l <- struct{}{}:
		case <-ctx.Done():
		}
	}
}

func (l Sem) Release() {
	if l != nil {
		select {
		case <-l:
		default:
		}
	}
}

// Chunk represents a segment of a chunked file that can be fetched on demand.
type Chunk[T any] struct {
	ID  T
	Idx int
	span

	refCnt  atomic.Int64
	ready   chan struct{}
	err     error
	content []byte
	lim     Sem
}

type ChunkedFile[T any] struct {
	name    string
	size    int
	chunks  []Chunk[T]
	fetcher func(id T) ([]byte, error)
}

// Size returns the total size of the file in bytes.
func (c *ChunkedFile[T]) Size() int {
	return c.size
}

// Name returns the file name.
func (c *ChunkedFile[T]) Name() string {
	return c.name
}

// reader returns a reader for the given span. It refs all chunks needed for
// the span; callers must Close the reader to release these refs.
//
// Concurrency: reader must not be called concurrently with FetchChunks. All
// readers should be opened before FetchChunks runs; no new readers should be
// opened until FetchChunks completes.
func (c *ChunkedFile[T]) reader(s span) io.ReadSeekCloser {
	// Find first chunk needed for span.
	start := 0
	if s.offset > 0 {
		start = sort.Search(len(c.chunks), func(i int) bool {
			return c.chunks[i].offset > s.offset
		}) - 1
	}

	// Increment reference counts for all chunks needed for span.
	for i := range c.chunks[start:] {
		if c.chunks[i+start].offset >= s.offset+s.length {
			break
		}
		c.chunks[i+start].ref()
	}

	return &chunkReader[T]{
		src:  c.chunks,
		span: s,
		cur:  start,
	}
}

// FetchChunks fetches all referenced chunks concurrently. It skips chunks that
// already have content or have no references.
//
// Concurrency: all readers should be opened before calling FetchChunks, and no
// new readers should be opened until it completes. See reader() for details.
func (c *ChunkedFile[T]) FetchChunks(ctx context.Context, lim Sem) error {
	g, ctx := errgroup.WithContext(ctx)
	done := ctx.Done()
	reqs := make(chan *Chunk[T], 1)

	for range 12 {
		g.Go(func() error {
			for {
				lim.Acquire(ctx)

				select {
				case <-done:
					return ctx.Err()
				case req, ok := <-reqs:
					if !ok {
						return nil
					}
					// Best-effort optimization: skip chunks no longer needed. This
					// is racy—if the chunk is unref'd during fetch, we'll complete
					// the fetch, set content, then immediately clear it when our
					// unref observes no remaining refs.
					if req.refCnt.Load() == 0 {
						continue
					}

					content, err := c.fetcher(req.ID)
					// Ref the chunk to prevent its count going to zero while we set
					// content. If all readers unref'd during the fetch (count went to
					// zero), this ref creates a new ready channel that our subsequent
					// unref will clear—this is fine since no readers are waiting.
					req.ref()
					if err != nil {
						req.setErr(err)
					} else {
						req.setContent(content)
					}
					req.unref()
				}
			}
		})
	}

	for i := range c.chunks {
		if c.chunks[i].ready == nil || c.chunks[i].content != nil {
			continue
		}
		select {
		case reqs <- &c.chunks[i]:
		case <-done:
			return ctx.Err()
		}
	}
	close(reqs)
	if err := g.Wait(); err != nil {	
		return err
	}
	return nil
}

type chunkReader[T any] struct {
	src  []Chunk[T]
	span span
	n    int
	cur  int
}

var _ io.ReadSeekCloser = (*chunkReader[any])(nil)

func (r *chunkReader[T]) Read(p []byte) (int, error) {
	// Check if we've read everything in the span.
	if r.n >= r.span.length {
		return 0, io.EOF
	}

	// Advance past chunks we've already consumed.
	for r.span.offset+r.n >= r.src[r.cur].offset+r.src[r.cur].length {
		if r.cur+1 >= len(r.src) {
			return 0, io.EOF
		}
		r.src[r.cur].unref()
		r.cur++
	}

	// Wait for the current chunk to be ready.
	select {
	case <-r.src[r.cur].ready:
	case <-time.After(5 * time.Minute):
		return 0, fmt.Errorf("timeout waiting for chunk %d", r.src[r.cur].Idx)
	}

	if r.src[r.cur].err != nil {
		return 0, r.src[r.cur].err
	}

	if len(r.src[r.cur].content) !=  r.src[r.cur].length {
		return 0, fmt.Errorf("chunk %d has incorrect content size %d != %d", 
			r.src[r.cur].Idx, len(r.src[r.cur].content), r.src[r.cur].length	)
	}

	// Calculate slice bounds within the current chunk.
	start := r.span.offset + r.n - r.src[r.cur].offset
	// End is the minimum of: chunk content length, or span end within this chunk.
	spanEndInChunk := r.span.offset + r.span.length - r.src[r.cur].offset
	end := min(len(r.src[r.cur].content), spanEndInChunk)

	if start >= end || start < 0 {
		return 0, fmt.Errorf("invalid read bounds: start=%d end=%d chunk=%d", start, end, r.src[r.cur].Idx)
	}

	n := copy(p, r.src[r.cur].content[start:end])
	r.n += n
	return n, nil
}

func (r *chunkReader[T]) Seek(n int64, wence int) (int64, error) {
	if wence != io.SeekCurrent {
		return 0, fmt.Errorf("unsupported seek")
	}
	r.n += int(n)
	return int64(r.n), nil
}

func (r *chunkReader[T]) Close() error {
	for i := r.cur; i < len(r.src) && r.src[i].offset < r.span.offset+r.span.length; i++ {
		r.src[i].unref()
	}
	return nil
}

func (c *Chunk[T]) setErr(err error) {
	c.err = err
	close(c.ready)
}

func (c *Chunk[T]) setContent(b []byte) {
	c.content = b
	close(c.ready)
}

// ref marks the chunk as referenced by a reader. The first ref (0→1) creates
// the ready channel that readers block on until content is set.
//
// Concurrency: ref is called during reader creation (before fetching) and by
// FetchChunks workers before setting content. These phases don't overlap, so
// ref calls don't race with each other or with unref.
func (c *Chunk[T]) ref() {
	if c.refCnt.Add(1) == 1 {
		c.ready = make(chan struct{})
	}
}

// unref releases a reader's reference to the chunk. When the last reference is
// released (count→0), the chunk's content and ready channel are cleared to
// allow garbage collection and prevent OOMs on large files.
//
// Concurrency: unref is called concurrently by reader goroutines as they finish
// reading individual chunks, and by FetchChunks workers after setting content.
// The atomic refCnt ensures exactly one caller sees count→0 and clears state.
func (c *Chunk[T]) unref() {
	if c.refCnt.Add(-1) == 0 {
		c.content = nil
		c.ready = nil
		c.lim.Release()
	}
}

