package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// TestNewChunkedFile_BasicConstruction tests creating a ChunkedFile with automatic offset calculation.
func TestNewChunkedFile_BasicConstruction(t *testing.T) {
	chunkIDs := []string{"chunk-0", "chunk-1", "chunk-2"}
	nominalSize := 100

	file := NewChunkedFile(
		"test.bin",
		250, // totalSize
		chunkIDs,
		nominalSize,
		nil, // fetcher
		nil, // no prefetched
	)

	if file.Name() != "test.bin" {
		t.Errorf("Expected name 'test.bin', got %q", file.Name())
	}

	if file.Size() != 250 {
		t.Errorf("Expected size 250, got %d", file.Size())
	}

	// Verify chunks were created with correct offsets/lengths
	if len(file.chunks) != 3 {
		t.Fatalf("Expected 3 chunks, got %d", len(file.chunks))
	}

	// Chunk 0: offset=0, length=100 (nominal)
	if file.chunks[0].offset != 0 || file.chunks[0].length != 100 {
		t.Errorf("Chunk 0: expected offset=0 length=100, got offset=%d length=%d",
			file.chunks[0].offset, file.chunks[0].length)
	}

	// Chunk 1: offset=100, length=100 (nominal)
	if file.chunks[1].offset != 100 || file.chunks[1].length != 100 {
		t.Errorf("Chunk 1: expected offset=100 length=100, got offset=%d length=%d",
			file.chunks[1].offset, file.chunks[1].length)
	}

	// Chunk 2 (last): offset=200, length=50 (remaining bytes)
	if file.chunks[2].offset != 200 || file.chunks[2].length != 50 {
		t.Errorf("Chunk 2: expected offset=200 length=50, got offset=%d length=%d",
			file.chunks[2].offset, file.chunks[2].length)
	}
}

// TestNewChunkedFile_WithPrefetched tests creating a ChunkedFile with prefetched chunks.
func TestNewChunkedFile_WithPrefetched(t *testing.T) {
	chunk0Data := bytes.Repeat([]byte("A"), 80) // Different from nominal
	chunk2Data := bytes.Repeat([]byte("C"), 60) // Last chunk

	prefetched := map[int][]byte{
		0: chunk0Data,
		2: chunk2Data,
	}

	chunkIDs := []string{"chunk-0", "chunk-1", "chunk-2"}
	file := NewChunkedFile(
		"test.bin",
		240, // 80 + 100 + 60
		chunkIDs,
		100, // nominal size
		nil,
		prefetched,
	)

	// Chunk 0: offset=0, length=80 (prefetched)
	if file.chunks[0].offset != 0 || file.chunks[0].length != 80 {
		t.Errorf("Chunk 0: expected offset=0 length=80, got offset=%d length=%d",
			file.chunks[0].offset, file.chunks[0].length)
	}
	if !bytes.Equal(file.chunks[0].content, chunk0Data) {
		t.Error("Chunk 0 content doesn't match prefetched data")
	}

	// Chunk 1: offset=80, length=100 (nominal, not prefetched)
	if file.chunks[1].offset != 80 || file.chunks[1].length != 100 {
		t.Errorf("Chunk 1: expected offset=80 length=100, got offset=%d length=%d",
			file.chunks[1].offset, file.chunks[1].length)
	}
	if file.chunks[1].content != nil {
		t.Error("Chunk 1 should not have content (not prefetched)")
	}

	// Chunk 2: offset=180, length=60 (prefetched last chunk)
	if file.chunks[2].offset != 180 || file.chunks[2].length != 60 {
		t.Errorf("Chunk 2: expected offset=180 length=60, got offset=%d length=%d",
			file.chunks[2].offset, file.chunks[2].length)
	}
	if !bytes.Equal(file.chunks[2].content, chunk2Data) {
		t.Error("Chunk 2 content doesn't match prefetched data")
	}
}

// TestChunkedFile_Reader_Prefetched tests reading from a file with prefetched chunks.
func TestChunkedFile_Reader_Prefetched(t *testing.T) {
	chunk0Data := bytes.Repeat([]byte("A"), 100)
	chunk1Data := bytes.Repeat([]byte("B"), 100)
	chunk2Data := bytes.Repeat([]byte("C"), 50)
	fullContent := append(append(chunk0Data, chunk1Data...), chunk2Data...)

	prefetched := map[int][]byte{
		0: chunk0Data,
		1: chunk1Data,
		2: chunk2Data,
	}

	chunkIDs := []string{"chunk-0", "chunk-1", "chunk-2"}
	file := NewChunkedFile(
		"test.bin",
		250,
		chunkIDs,
		100,
		func(id string) ([]byte, error) {
			return nil, errors.New("should not be called - all chunks prefetched")
		},
		prefetched,
	)

	// Create reader for entire file
	reader := file.Reader(0, 250)
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

// TestChunkedFile_FetchChunks tests fetching chunks on demand.
func TestChunkedFile_FetchChunks(t *testing.T) {
	chunk1Data := bytes.Repeat([]byte("B"), 100)

	fetchedChunks := make(map[string]bool)

	chunkIDs := []string{"chunk-0", "chunk-1", "chunk-2"}
	prefetched := map[int][]byte{
		0: bytes.Repeat([]byte("A"), 100), // Prefetch first chunk
		2: bytes.Repeat([]byte("C"), 50),  // Prefetch last chunk
	}

	file := NewChunkedFile(
		"test.bin",
		250,
		chunkIDs,
		100,
		func(id string) ([]byte, error) {
			fetchedChunks[id] = true
			if id == "chunk-1" {
				return chunk1Data, nil
			}
			return nil, errors.New("unexpected chunk ID")
		},
		prefetched,
	)

	// Open reader spanning all chunks
	reader := file.Reader(0, 250)
	defer reader.Close()

	// Fetch chunks
	ctx := context.Background()
	if err := file.FetchChunks(ctx, nil); err != nil {
		t.Fatalf("FetchChunks failed: %v", err)
	}

	// Verify only chunk-1 was fetched (chunk-0 and chunk-2 were prefetched)
	if !fetchedChunks["chunk-1"] {
		t.Error("Expected chunk-1 to be fetched")
	}
	if fetchedChunks["chunk-0"] || fetchedChunks["chunk-2"] {
		t.Error("chunk-0 and chunk-2 should not have been fetched (already prefetched)")
	}

	// Read the file to verify content
	buf, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	if len(buf) != 250 {
		t.Errorf("Expected 250 bytes, got %d", len(buf))
	}
}

// TestChunkedFile_PinReadChunks tests pinning mode.
func TestChunkedFile_PinReadChunks(t *testing.T) {
	chunk0Data := bytes.Repeat([]byte("A"), 100)
	chunk1Data := bytes.Repeat([]byte("B"), 100)

	prefetched := map[int][]byte{
		0: chunk0Data,
		1: chunk1Data,
	}

	chunkIDs := []string{"chunk-0", "chunk-1"}
	file := NewChunkedFile(
		"test.bin",
		200,
		chunkIDs,
		100,
		nil,
		prefetched,
	)

	// Enable pinning mode
	cleanup := file.PinReadChunks()
	defer cleanup()

	// Verify pinning is enabled
	if !file.pinning {
		t.Error("Expected pinning to be enabled")
	}

	// Read from file
	reader := file.Reader(0, 200)
	buf, err := io.ReadAll(reader)
	reader.Close()

	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	if len(buf) != 200 {
		t.Errorf("Expected 200 bytes, got %d", len(buf))
	}

	// Chunks should still have content (not cleared due to pinning)
	if file.chunks[0].content == nil {
		t.Error("Chunk 0 content was cleared despite pinning")
	}
	if file.chunks[1].content == nil {
		t.Error("Chunk 1 content was cleared despite pinning")
	}

	// Cleanup should disable pinning
	cleanup()
	if file.pinning {
		t.Error("Expected pinning to be disabled after cleanup")
	}
}

// TestChunkedFile_Reader_PartialRead tests reading a subset of the file.
func TestChunkedFile_Reader_PartialRead(t *testing.T) {
	chunk0Data := bytes.Repeat([]byte("A"), 100)
	chunk1Data := bytes.Repeat([]byte("B"), 100)
	chunk2Data := bytes.Repeat([]byte("C"), 100)

	prefetched := map[int][]byte{
		0: chunk0Data,
		1: chunk1Data,
		2: chunk2Data,
	}

	chunkIDs := []string{"chunk-0", "chunk-1", "chunk-2"}
	file := NewChunkedFile(
		"test.bin",
		300,
		chunkIDs,
		100,
		nil,
		prefetched,
	)

	// Read from middle of chunk0 to middle of chunk1
	reader := file.Reader(50, 100)
	defer reader.Close()

	buf, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	// Should read 50 bytes of 'A' (from offset 50 to 100) + 50 bytes of 'B' (from offset 100 to 150)
	expected := append(bytes.Repeat([]byte("A"), 50), bytes.Repeat([]byte("B"), 50)...)
	if !bytes.Equal(buf, expected) {
		t.Errorf("Partial read content doesn't match expected. Got %d bytes, expected %d bytes", len(buf), len(expected))
	}
}

// TestChunkedFile_LargeFile tests a file with many chunks.
func TestChunkedFile_LargeFile(t *testing.T) {
	numChunks := 10
	chunkIDs := make([]string, numChunks)
	for i := range chunkIDs {
		chunkIDs[i] = string(rune('A' + i))
	}

	file := NewChunkedFile(
		"test.bin",
		numChunks*100,
		chunkIDs,
		100,
		func(id string) ([]byte, error) {
			// Return chunk data matching the ID
			return bytes.Repeat([]byte(id), 100), nil
		},
		nil,
	)

	// Open reader for all chunks
	reader := file.Reader(0, numChunks*100)
	defer reader.Close()

	// Fetch all chunks
	ctx := context.Background()
	if err := file.FetchChunks(ctx, nil); err != nil {
		t.Fatalf("FetchChunks failed: %v", err)
	}

	// Read all content
	buf, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	if len(buf) != numChunks*100 {
		t.Errorf("Expected %d bytes, got %d", numChunks*100, len(buf))
	}
}
