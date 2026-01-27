package stream

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"

	"github.com/dt/gosendsafely/util"
	"golang.org/x/sync/errgroup"
)

type span struct {
	offset int
	length int
}

// chunk represents a segment of a chunked file that can be fetched on demand.
// All fields are private to hide implementation details.
type chunk[T any] struct {
	id  T
	idx int
	span

	refCnt  atomic.Int64
	ready   chan struct{}
	err     error
	content []byte
	lim     util.Sem
}

type ChunkedFile[T any] struct {
	name    string
	size    int
	chunks  []chunk[T]
	fetcher func(id T) ([]byte, error)
	pinning bool // If true, chunks are double-ref'd to pin them in memory
}

// NewChunkedFile creates a chunked file from chunk IDs.
// - name: file name
// - totalSize: total file size in bytes
// - chunkIDs: slice of chunk identifiers (length = number of chunks)
// - nominalChunkSize: standard chunk size (all chunks except last and prefetched)
// - fetcher: function to fetch chunk content by ID
// - prefetched: optional map of pre-fetched chunk content (index -> bytes)
//
// Chunk sizes are determined as:
// - If chunk index is in prefetched map: size = len(prefetched[idx])
// - Else if chunk is last: size = remaining bytes to reach totalSize
// - Else: size = nominalChunkSize
//
// Offsets are calculated automatically based on chunk sizes.
func NewChunkedFile[T any](
	name string,
	totalSize int,
	chunkIDs []T,
	nominalChunkSize int,
	fetcher func(T) ([]byte, error),
	prefetched map[int][]byte,
) *ChunkedFile[T] {
	chunks := make([]chunk[T], len(chunkIDs))
	offset := 0

	for i := range chunks {
		chunks[i].id = chunkIDs[i]
		chunks[i].idx = i
		chunks[i].offset = offset

		// Determine chunk length
		if content, ok := prefetched[i]; ok {
			// Prefetched chunk - use actual content size
			chunks[i].length = len(content)
			chunks[i].content = content
			chunks[i].refCnt.Store(1)         // Mark as pinned
			chunks[i].ready = make(chan struct{})
			close(chunks[i].ready) // Already has content
		} else if i == len(chunkIDs)-1 {
			// Last chunk - remaining bytes
			chunks[i].length = totalSize - offset
		} else {
			// Normal chunk - use nominal size
			chunks[i].length = nominalChunkSize
		}

		offset += chunks[i].length
	}

	return &ChunkedFile[T]{
		name:    name,
		size:    totalSize,
		chunks:  chunks,
		fetcher: fetcher,
	}
}

// Size returns the total size of the file in bytes.
func (c *ChunkedFile[T]) Size() int {
	return c.size
}

// Name returns the file name.
func (c *ChunkedFile[T]) Name() string {
	return c.name
}

// Reader returns a reader for the given offset and length. It refs all chunks
// needed for the span; callers must Close the reader to release these refs.
// If pinning mode is active, chunks are double-ref'd to keep them in memory.
//
// Concurrency: Reader must not be called concurrently with FetchChunks. All
// readers should be opened before FetchChunks runs; no new readers should be
// opened until FetchChunks completes.
func (c *ChunkedFile[T]) Reader(offset, length int) io.ReadSeekCloser {
	s := span{offset: offset, length: length}

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
		// If pinning mode is active, double-ref to pin the chunk
		if c.pinning {
			c.chunks[i+start].ref()
		}
	}

	return &chunkReader[T]{
		src:  c.chunks,
		span: s,
		cur:  start,
	}
}

// PinReadChunks enables pinning mode for all readers opened until the returned
// cleanup function is called. In pinning mode, chunks are double-ref'd to keep
// them in memory for repeated reads (e.g., ZIP recovery).
// Usage: defer file.PinReadChunks()()
func (c *ChunkedFile[T]) PinReadChunks() func() {
	c.pinning = true
	return func() {
		c.pinning = false
	}
}

// FetchChunks fetches all referenced chunks concurrently. It skips chunks that
// already have content or have no references.
//
// Concurrency: all readers should be opened before calling FetchChunks, and no
// new readers should be opened until it completes. See Reader() for details.
func (c *ChunkedFile[T]) FetchChunks(ctx context.Context, lim util.Sem) error {
	g, ctx := errgroup.WithContext(ctx)
	done := ctx.Done()
	reqs := make(chan *chunk[T], 1)

	for range 12 {
		g.Go(func() error {
			for {
				lim.Acquire(ctx)

				select {
				case <-done:
					lim.Release()
					return ctx.Err()
				case req, ok := <-reqs:
					if !ok {
						lim.Release()
						return nil
					}
					// Best-effort optimization: skip chunks no longer needed. This
					// is racy—if the chunk is unref'd during fetch, we'll complete
					// the fetch, set content, then immediately clear it when our
					// unref observes no remaining refs.
					if req.refCnt.Load() == 0 {
						lim.Release()
						continue
					}

					// Set lim so unref() can release when chunk is consumed
					req.lim = lim
					content, err := c.fetcher(req.id)
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
	src  []chunk[T]
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
		return 0, fmt.Errorf("timeout waiting for chunk %d", r.src[r.cur].idx)
	}

	if r.src[r.cur].err != nil {
		return 0, r.src[r.cur].err
	}

	if len(r.src[r.cur].content) != r.src[r.cur].length {
		return 0, fmt.Errorf("chunk %d has incorrect content size %d != %d",
			r.src[r.cur].idx, len(r.src[r.cur].content), r.src[r.cur].length)
	}

	// Calculate slice bounds within the current chunk.
	start := r.span.offset + r.n - r.src[r.cur].offset
	// End is the minimum of: chunk content length, or span end within this chunk.
	spanEndInChunk := r.span.offset + r.span.length - r.src[r.cur].offset
	end := min(len(r.src[r.cur].content), spanEndInChunk)

	if start >= end || start < 0 {
		return 0, fmt.Errorf("invalid read bounds: start=%d end=%d chunk=%d", start, end, r.src[r.cur].idx)
	}

	n := copy(p, r.src[r.cur].content[start:end])
	r.n += n
	return n, nil
}

func (r *chunkReader[T]) Seek(n int64, whence int) (int64, error) {
	if whence != io.SeekCurrent {
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

func (c *chunk[T]) setErr(err error) {
	c.err = err
	close(c.ready)
}

func (c *chunk[T]) setContent(b []byte) {
	c.content = b
	close(c.ready)
}

// ref marks the chunk as referenced by a reader. The first ref (0→1) creates
// the ready channel that readers block on until content is set.
//
// Concurrency: ref is called during reader creation (before fetching) and by
// FetchChunks workers before setting content. These phases don't overlap, so
// ref calls don't race with each other or with unref.
func (c *chunk[T]) ref() {
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
func (c *chunk[T]) unref() {
	if c.refCnt.Add(-1) == 0 {
		c.content = nil
		c.ready = nil
		c.lim.Release()
	}
}
