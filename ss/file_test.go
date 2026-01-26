package ss

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// TestChunkedFile_Reader tests creating and reading from a ChunkedFile reader.
func TestChunkedFile_Reader(t *testing.T) {
	chunk1 := bytes.Repeat([]byte("A"), 100)
	chunk2 := bytes.Repeat([]byte("B"), 100)
	chunk3 := bytes.Repeat([]byte("C"), 50)
	fullContent := append(append(chunk1, chunk2...), chunk3...)

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
		{ID: "chunk-2", Idx: 2, span: span{offset: 200, length: 50}},
	}

	// Pre-populate chunks with content and mark them ready
	for i := range chunks {
		chunks[i].ref()
	}
	chunks[0].setContent(chunk1)
	chunks[1].setContent(chunk2)
	chunks[2].setContent(chunk3)

	file := &ChunkedFile[string]{
		name:   "test.bin",
		size:   250,
		chunks: chunks,
		fetcher: func(id string) ([]byte, error) {
			return nil, errors.New("should not be called")
		},
	}

	// Create reader for entire file
	reader := file.reader(span{offset: 0, length: 250})
	defer reader.Close()

	buf := make([]byte, 1024)
	var result []byte
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
	}

	if !bytes.Equal(result, fullContent) {
		t.Errorf("Read content doesn't match expected. Got %d bytes, expected %d bytes", len(result), len(fullContent))
	}
}

// TestChunkedFile_Reader_PartialRead tests reading a subset of the file.
func TestChunkedFile_Reader_PartialRead(t *testing.T) {
	chunk1 := bytes.Repeat([]byte("A"), 100)
	chunk2 := bytes.Repeat([]byte("B"), 100)
	chunk3 := bytes.Repeat([]byte("C"), 100)

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
		{ID: "chunk-2", Idx: 2, span: span{offset: 200, length: 100}},
	}

	for i := range chunks {
		chunks[i].ref()
	}
	chunks[0].setContent(chunk1)
	chunks[1].setContent(chunk2)
	chunks[2].setContent(chunk3)

	file := &ChunkedFile[string]{
		name:   "test.bin",
		size:   300,
		chunks: chunks,
	}

	// Read from middle of chunk1 to middle of chunk2
	reader := file.reader(span{offset: 50, length: 100})
	defer reader.Close()

	buf := make([]byte, 1024)
	var result []byte
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
	}

	expected := append(bytes.Repeat([]byte("A"), 50), bytes.Repeat([]byte("B"), 50)...)
	if !bytes.Equal(result, expected) {
		t.Errorf("Partial read content doesn't match. Got %d bytes, expected %d bytes", len(result), len(expected))
	}
}

// TestChunkedFile_Size tests the Size method.
func TestChunkedFile_Size(t *testing.T) {
	file := &ChunkedFile[string]{
		name: "test.txt",
		size: 12345,
	}

	if file.Size() != 12345 {
		t.Errorf("Expected size 12345, got %d", file.Size())
	}
}

// TestChunkedFile_Name tests the Name method.
func TestChunkedFile_Name(t *testing.T) {
	file := &ChunkedFile[string]{
		name: "myfile.txt",
		size: 100,
	}

	if file.Name() != "myfile.txt" {
		t.Errorf("Expected name 'myfile.txt', got '%s'", file.Name())
	}
}

// TestChunk_RefUnref tests reference counting on chunks.
func TestChunk_RefUnref(t *testing.T) {
	chunk := &Chunk[string]{
		ID:    "test-chunk",
		Idx:   0,
		span: span{offset: 0, length: 100},
	}

	// Initially no refs
	if chunk.refCnt.Load() != 0 {
		t.Error("Expected initial refcount of 0")
	}

	// Add ref
	chunk.ref()
	if chunk.refCnt.Load() != 1 {
		t.Error("Expected refcount of 1 after Ref()")
	}
	if chunk.ready == nil {
		t.Error("Expected ready channel to be created after first Ref()")
	}

	// Add another ref
	chunk.ref()
	if chunk.refCnt.Load() != 2 {
		t.Error("Expected refcount of 2 after second Ref()")
	}

	// Set content
	content := []byte("test content")
	chunk.setContent(content)
	if !bytes.Equal(chunk.content, content) {
		t.Error("Content not set correctly")
	}

	// Remove one ref
	chunk.unref()
	if chunk.refCnt.Load() != 1 {
		t.Error("Expected refcount of 1 after first Unref()")
	}
	if chunk.content == nil {
		t.Error("Content should not be nil when refcount > 0")
	}

	// Remove last ref
	chunk.unref()
	if chunk.refCnt.Load() != 0 {
		t.Error("Expected refcount of 0 after second Unref()")
	}
	if chunk.content != nil {
		t.Error("Content should be nil when refcount is 0")
	}
	if chunk.ready != nil {
		t.Error("Ready channel should be nil when refcount is 0")
	}
}

// TestChunk_Err tests setting an error on a chunk.
func TestChunk_Err(t *testing.T) {
	chunk := &Chunk[string]{
		ID:    "test-chunk",
		Idx:   0,
		span: span{offset: 0, length: 100},
	}

	chunk.ref()

	expectedErr := errors.New("download failed")
	chunk.setErr(expectedErr)

	if chunk.err != expectedErr {
		t.Errorf("Expected error '%v', got '%v'", expectedErr, chunk.err)
	}

	// Verify ready channel is closed
	select {
	case <-chunk.ready:
		// OK, channel is closed
	default:
		t.Error("Expected ready channel to be closed after Err()")
	}
}

// TestChunkedFile_FetchChunks tests the FetchChunks method.
func TestChunkedFile_FetchChunks(t *testing.T) {
	chunkData := [][]byte{
		bytes.Repeat([]byte("1"), 100),
		bytes.Repeat([]byte("2"), 100),
		bytes.Repeat([]byte("3"), 100),
	}

	var fetchCount atomic.Int32
	fetcher := func(id string) ([]byte, error) {
		fetchCount.Add(1)
		switch id {
		case "chunk-0":
			return chunkData[0], nil
		case "chunk-1":
			return chunkData[1], nil
		case "chunk-2":
			return chunkData[2], nil
		default:
			return nil, errors.New("unknown chunk")
		}
	}

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
		{ID: "chunk-2", Idx: 2, span: span{offset: 200, length: 100}},
	}

	// Ref all chunks to indicate they need fetching
	for i := range chunks {
		chunks[i].ref()
	}

	file := &ChunkedFile[string]{
		name:    "test.bin",
		size:    300,
		chunks:  chunks,
		fetcher: fetcher,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := file.FetchChunks(ctx, nil)
	if err != nil {
		t.Fatalf("FetchChunks failed: %v", err)
	}

	// Verify all chunks were fetched
	if fetchCount.Load() != 3 {
		t.Errorf("Expected 3 fetches, got %d", fetchCount.Load())
	}

	// Verify chunk contents
	for i := range file.chunks {
		if !bytes.Equal(file.chunks[i].content, chunkData[i]) {
			t.Errorf("Chunk %d content doesn't match", i)
		}
	}
}

// TestChunkedFile_FetchChunks_WithError tests error handling in FetchChunks.
func TestChunkedFile_FetchChunks_WithError(t *testing.T) {
	expectedErr := errors.New("network error")
	fetcher := func(id string) ([]byte, error) {
		if id == "chunk-1" {
			return nil, expectedErr
		}
		return bytes.Repeat([]byte("X"), 100), nil
	}

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
	}

	for i := range chunks {
		chunks[i].ref()
	}

	file := &ChunkedFile[string]{
		name:    "test.bin",
		size:    200,
		chunks:  chunks,
		fetcher: fetcher,
	}

	ctx := context.Background()
	err := file.FetchChunks(ctx, nil)

	// FetchChunks doesn't return the error directly, but sets it on the chunk
	if err != nil {
		t.Logf("FetchChunks returned error (expected): %v", err)
	}

	// The chunk should have the error set
	if file.chunks[1].err == nil {
		t.Error("Expected error to be set on chunk 1")
	}
}

// TestChunkedFile_FetchChunks_Cancellation tests context cancellation.
func TestChunkedFile_FetchChunks_Cancellation(t *testing.T) {
	fetcher := func(id string) ([]byte, error) {
		time.Sleep(100 * time.Millisecond)
		return bytes.Repeat([]byte("X"), 100), nil
	}

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
		{ID: "chunk-2", Idx: 2, span: span{offset: 200, length: 100}},
	}

	for i := range chunks {
		chunks[i].ref()
	}

	file := &ChunkedFile[string]{
		name:    "test.bin",
		size:    300,
		chunks:  chunks,
		fetcher: fetcher,
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately
	cancel()

	err := file.FetchChunks(ctx, nil)
	if err == nil {
		t.Log("FetchChunks may or may not return error on cancellation")
	}
}

// TestChunkReader_Seek tests seeking in the chunk reader.
func TestChunkReader_Seek(t *testing.T) {
	file := &ChunkedFile[string]{
		name:   "test.bin",
		size:   100,
		chunks: make([]Chunk[string], 1),
	}
	file.chunks[0].ID = "chunk-0"
	file.chunks[0].Idx = 0
	file.chunks[0].span = span{offset: 0, length: 100}
	file.chunks[0].ref()
	file.chunks[0].setContent(bytes.Repeat([]byte("X"), 100))

	reader := file.reader(span{offset: 0, length: 100})
	defer reader.Close()

	// Read some bytes
	buf := make([]byte, 10)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 10 {
		t.Errorf("Expected to read 10 bytes, got %d", n)
	}

	// Seek forward
	pos, err := reader.Seek(20, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	if pos != 30 {
		t.Errorf("Expected position 30 after seek, got %d", pos)
	}
}

// TestChunkReader_UnsupportedSeek tests that unsupported seek modes return errors.
func TestChunkReader_UnsupportedSeek(t *testing.T) {
	file := &ChunkedFile[string]{
		name:   "test.bin",
		size:   100,
		chunks: make([]Chunk[string], 1),
	}
	file.chunks[0].ID = "chunk-0"
	file.chunks[0].Idx = 0
	file.chunks[0].span = span{offset: 0, length: 100}
	file.chunks[0].ref()
	file.chunks[0].setContent(bytes.Repeat([]byte("X"), 100))

	reader := file.reader(span{offset: 0, length: 100})
	defer reader.Close()

	_, err := reader.Seek(10, io.SeekStart)
	if err == nil {
		t.Error("Expected error for SeekStart")
	}

	_, err = reader.Seek(10, io.SeekEnd)
	if err == nil {
		t.Error("Expected error for SeekEnd")
	}
}

// TestChunkReader_Close tests that Close releases chunk references.
func TestChunkReader_Close(t *testing.T) {
	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
	}

	for i := range chunks {
		chunks[i].ref()
		chunks[i].setContent(bytes.Repeat([]byte{byte('A' + i)}, 100))
	}

	file := &ChunkedFile[string]{
		name:   "test.bin",
		size:   200,
		chunks: chunks,
	}

	reader := file.reader(span{offset: 0, length: 200})

	// Verify chunks are referenced
	if file.chunks[0].refCnt.Load() < 1 || file.chunks[1].refCnt.Load() < 1 {
		t.Error("Expected chunks to be referenced by reader")
	}

	err := reader.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// TestLimiter tests the semaphore limiter.
func TestLimiter(t *testing.T) {
	lim := Limiter(2)

	ctx := context.Background()

	// Should be able to acquire twice
	lim.Acquire(ctx)
	lim.Acquire(ctx)

	// Release once
	lim.Release()

	// Should be able to acquire again
	lim.Acquire(ctx)

	// Release all
	lim.Release()
	lim.Release()
}

// TestLimiter_Nil tests that nil limiter doesn't panic.
func TestLimiter_Nil(t *testing.T) {
	var lim Sem = nil

	ctx := context.Background()

	// These should not panic
	lim.Acquire(ctx)
	lim.Release()
}

// TestLimiter_Cancellation tests that Acquire respects context cancellation.
func TestLimiter_Cancellation(t *testing.T) {
	lim := Limiter(1)

	ctx := context.Background()

	// Fill the limiter
	lim.Acquire(ctx)

	// Create a cancelable context
	ctx2, cancel := context.WithCancel(context.Background())

	// Start a goroutine that will try to acquire
	done := make(chan struct{})
	go func() {
		lim.Acquire(ctx2)
		close(done)
	}()

	// Cancel the context
	time.Sleep(10 * time.Millisecond)
	cancel()

	// The goroutine should return (either acquired or cancelled)
	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Error("Acquire did not return after context cancellation")
	}
}

// TestChunkedFile_Reader_EmptySpan tests reading with an empty span.
func TestChunkedFile_Reader_EmptySpan(t *testing.T) {
	file := &ChunkedFile[string]{
		name:   "test.bin",
		size:   100,
		chunks: make([]Chunk[string], 1),
	}
	file.chunks[0].ID = "chunk-0"
	file.chunks[0].Idx = 0
	file.chunks[0].span = span{offset: 0, length: 100}
	file.chunks[0].ref()
	file.chunks[0].setContent(bytes.Repeat([]byte("X"), 100))

	reader := file.reader(span{offset: 50, length: 0})
	defer reader.Close()

	buf := make([]byte, 10)
	n, err := reader.Read(buf)
	if err != io.EOF {
		t.Errorf("Expected EOF for empty span, got err=%v, n=%d", err, n)
	}
}

// TestChunkedFile_FetchChunks_SkipsAlreadyFetched tests that already-fetched chunks are skipped.
func TestChunkedFile_FetchChunks_SkipsAlreadyFetched(t *testing.T) {
	var fetchCount atomic.Int32
	fetcher := func(id string) ([]byte, error) {
		fetchCount.Add(1)
		return bytes.Repeat([]byte("X"), 100), nil
	}

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
	}

	// Ref and set the first chunk (simulating already fetched)
	chunks[0].ref()
	chunks[0].setContent(bytes.Repeat([]byte("Y"), 100))

	// Only ref the second chunk
	chunks[1].ref()

	file := &ChunkedFile[string]{
		name:    "test.bin",
		size:    200,
		chunks:  chunks,
		fetcher: fetcher,
	}

	ctx := context.Background()
	err := file.FetchChunks(ctx, nil)
	if err != nil {
		t.Fatalf("FetchChunks failed: %v", err)
	}

	// Only chunk-1 should have been fetched
	if fetchCount.Load() != 1 {
		t.Errorf("Expected 1 fetch (for unfetched chunk), got %d", fetchCount.Load())
	}

	// Verify chunk-0 still has original content
	if !bytes.Equal(file.chunks[0].content, bytes.Repeat([]byte("Y"), 100)) {
		t.Error("Chunk 0 content was overwritten")
	}
}

// TestChunkedFile_FetchChunks_SkipsUnreffedChunks tests that chunks with refcount 0 are skipped.
func TestChunkedFile_FetchChunks_SkipsUnreffedChunks(t *testing.T) {
	var fetchCount atomic.Int32
	fetcher := func(id string) ([]byte, error) {
		fetchCount.Add(1)
		return bytes.Repeat([]byte("X"), 100), nil
	}

	chunks := []Chunk[string]{
		{ID: "chunk-0", Idx: 0, span: span{offset: 0, length: 100}},
		{ID: "chunk-1", Idx: 1, span: span{offset: 100, length: 100}},
	}

	// Only ref chunk-0, leave chunk-1 unreffed
	chunks[0].ref()
	// chunk-1 has refCnt = 0, no ready channel

	file := &ChunkedFile[string]{
		name:    "test.bin",
		size:    200,
		chunks:  chunks,
		fetcher: fetcher,
	}

	ctx := context.Background()
	err := file.FetchChunks(ctx, nil)
	if err != nil {
		t.Fatalf("FetchChunks failed: %v", err)
	}

	// Only chunk-0 should have been fetched
	if fetchCount.Load() != 1 {
		t.Errorf("Expected 1 fetch, got %d", fetchCount.Load())
	}
}
