package ziputil

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dt/gosendsafely/stream"
	"github.com/dt/gosendsafely/util"
	"golang.org/x/sync/errgroup"
)

type extraction struct {
	src         io.ReadSeekCloser
	out         string
	srcSize     int64
	compression uint16
}

// Extract extracts files from the index to dest, skipping files that already
// exist with the correct size. Returns the number of skipped files and their
// total size.
func Extract[T any](
	src *stream.ChunkedFile[T], index ZipIndex, dest string, lim util.Sem, progress func(float64, util.BytesSize),
) (int, util.BytesSize, error) {
	extractions := make(chan extraction, len(index))

	var totalSize int
	var existingSize, existingFiles util.BytesSize
	for _, i := range index {
		totalSize += int(i.CompressedSize())
		// Expand range to include local file header (30 bytes + filename + extra field)
		headerEstimate := 30 + len(i.Name) + 256
		expandedSpan := span{offset: i.src.offset, length: i.src.length + headerEstimate}
		out := filepath.Join(dest, i.Name)
		if s, err := os.Stat(out); err == nil && s.Size() == int64(i.Size) {
			// File already exists with correct size, skip extraction
			existingFiles++
			existingSize += util.BytesSize(i.CompressedSize())
			continue
		}
		extractions <- extraction{
			src:         src.Reader(expandedSpan.offset, expandedSpan.length),
			out:         out,
			srcSize:     int64(i.CompressedSize()),
			compression: i.compression,
		}
	}
	close(extractions)

	var finished atomic.Int64
	finished.Add(int64(existingSize))
	g, ctx := errgroup.WithContext(context.Background())
	done := ctx.Done()
	poll := make(chan struct{})
	g.Go(func() error {
		defer close(poll)
		return src.FetchChunks(ctx, lim)
	})

	for range 4 {
		g.Go(func() error {
			for {
				select {
				case <-done:
					return ctx.Err()
				case ex, ok := <-extractions:
					if !ok {
						return nil
					}
					if err := ex.write(); err != nil {
						return err
					}
					finished.Add(ex.srcSize)
				}
			}
		})
	}

	lastProg := util.BytesSize(finished.Load())
	lastReport := time.Now()
	for {
		select {
		case <-poll:
			return int(existingFiles), existingSize, g.Wait()
		case <-time.After(time.Second):
			if progress != nil {
				cur := util.BytesSize(finished.Load())
				delta := util.BytesSize(float64(cur-lastProg) / time.Since(lastReport).Seconds())
				lastProg = cur
				lastReport = time.Now()
				progress(float64(cur)/float64(totalSize), delta)
			}
		}
	}
}

func (e *extraction) write() error {
	defer e.src.Close()

	var header [30]byte
	_, err := io.ReadFull(e.src, header[:])
	if err != nil {
		return err
	}

	// Verify signature
	if !bytes.Equal(header[0:4], fileSig) {
		return fmt.Errorf("invalid local file header signature")
	}

	// Decode variable field lengths from local header (may differ from CD).
	localFileNameLen := uint16(header[26]) | uint16(header[27])<<8
	localExtraLen := uint16(header[28]) | uint16(header[29])<<8

	// Advance past variable fields to start of file data.
	skip := localExtraLen + localFileNameLen
	if skip > 0 {
		_, err := e.src.Seek(int64(skip), io.SeekCurrent)
		if err != nil {
			return err
		}
	}

	// Create an output tmp file.
	if err := os.MkdirAll(filepath.Dir(e.out), 0755); err != nil {
		return err
	}
	tmpName := e.out + ".download-tmp"

	f, err := os.Create(tmpName)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	// Copy the file data, decompressing if needed, to the output file.
	switch e.compression {
	case 0: // Stored
		_, err = io.Copy(f, e.src)
	case 8: // Deflate
		ex := flate.NewReader(e.src)
		defer ex.Close()
		_, err = io.Copy(f, ex)
	default:
		return fmt.Errorf("unsupported compression method %d", e.compression)
	}
	if err == nil {
		err = f.Close()
		f = nil
	}

	if err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, e.out)
}
