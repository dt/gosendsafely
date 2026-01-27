package ziputil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dt/gosendsafely/stream"
	"github.com/dt/gosendsafely/util"
)

var (
	eocdSig      = []byte{0x50, 0x4b, 0x05, 0x06}
	fileSig      = []byte{0x50, 0x4b, 0x03, 0x04}
	cdSig        = []byte{0x50, 0x4b, 0x01, 0x02}
	zip64LocSig  = []byte{0x50, 0x4b, 0x06, 0x07}
	zip64EOCDSig = []byte{0x50, 0x4b, 0x06, 0x06}
)

type span struct {
	offset int
	length int
}

// ZippedFile represents a file entry in a ZIP archive.
type ZippedFile struct {
	Name        string
	Size        util.BytesSize
	src         span
	compression uint16
}

// CompressedSize returns the compressed size of the file in the archive.
func (z *ZippedFile) CompressedSize() util.BytesSize {
	return util.BytesSize(z.src.length)
}

// ZipIndex is a list of files in a ZIP archive.
type ZipIndex []ZippedFile

// DecodeIndex reads the ZIP central directory from a chunked file and returns
// an index of all files in the archive.
func DecodeIndex[T any](src *stream.ChunkedFile[T]) (ZipIndex, error) {
	// Step 1: Read last 4KB to find EOCD
	const initialTailSize = 4 << 10 // 4KB
	tailSize := min(initialTailSize, src.Size())

	tail, err := readRange(src, src.Size()-tailSize, tailSize)
	if err != nil {
		return nil, fmt.Errorf("failed to read tail: %w", err)
	}

	// Look for EOCD
	eocdPos := findEOCD(tail)
	if eocdPos < 0 {
		// No EOCD found - try recovery mode for truncated ZIPs
		fmt.Fprintln(os.Stderr, "WARNING: ZIP appears truncated (no EOCD found), attempting recovery...")
		return recoverCDIndex(src)
	}

	// Parse EOCD to get CD location
	cdOffset, cdSize, err := parseEOCD(tail, eocdPos, src.Size()-tailSize, src.Size())
	if err != nil {
		return nil, err
	}

	// Handle ZIP64 - need more data
	if cdOffset == 0xFFFFFFFF || cdSize == 0xFFFFFFFF {
		// Expand read to include ZIP64 structures (need ~100 bytes before EOCD)
		expandedSize := min(tailSize+1024, src.Size())
		if expandedSize > tailSize {
			tail, err = readRange(src, src.Size()-expandedSize, expandedSize)
			if err != nil {
				return nil, fmt.Errorf("failed to read expanded tail for ZIP64: %w", err)
			}
			eocdPos = findEOCD(tail)
			if eocdPos < 0 {
				return nil, fmt.Errorf("lost EOCD after expanding read")
			}
		}

		cdOffset, cdSize, err = parseZip64EOCD(tail, eocdPos, src.Size()-len(tail))
		if err != nil {
			return nil, err
		}
	}

	// Step 2: Read exact CD range
	if cdOffset < 0 || cdSize <= 0 {
		return nil, fmt.Errorf("invalid CD offset/size: offset=%d size=%d", cdOffset, cdSize)
	}

	// Check if CD is already in our tail buffer
	bufferStart := src.Size() - len(tail)
	cdLocalOffset := cdOffset - bufferStart

	var cdData []byte
	if cdLocalOffset >= 0 && cdLocalOffset+cdSize <= len(tail) {
		// CD is in our buffer
		cdData = tail[cdLocalOffset : cdLocalOffset+cdSize]
	} else {
		// Need to fetch CD separately
		cdData, err = readRange(src, cdOffset, cdSize)
		if err != nil {
			return nil, fmt.Errorf("failed to read Central Directory: %w", err)
		}
	}

	return parseCDEntries(cdData)
}

// readRange reads a range of bytes from the chunked file.
func readRange[T any](src *stream.ChunkedFile[T], offset, length int) ([]byte, error) {
	// Enable pinning mode to prevent chunk eviction for potential re-reads during recovery
	defer src.PinReadChunks()()

	buf := make([]byte, length)
	r := src.Reader(offset, length)
	defer r.Close()

	if err := src.FetchChunks(context.Background(), nil); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// findEOCD searches backwards for the EOCD signature in a buffer.
// Returns the position of the signature, or -1 if not found.
func findEOCD(buf []byte) int {
	for i := len(buf) - 22; i >= 0; i-- {
		if bytes.Equal(buf[i:i+4], eocdSig) {
			return i
		}
	}
	return -1
}

// parseEOCD extracts CD offset and size from EOCD record.
func parseEOCD(buf []byte, eocdPos, bufferStartOffset, fileSize int) (cdOffset, cdSize int, err error) {
	if eocdPos+22 > len(buf) {
		return 0, 0, fmt.Errorf("EOCD too short")
	}
	eocd := buf[eocdPos:]
	cdSize = int(uint32(eocd[12]) | uint32(eocd[13])<<8 | uint32(eocd[14])<<16 | uint32(eocd[15])<<24)
	cdOffset = int(uint32(eocd[16]) | uint32(eocd[17])<<8 | uint32(eocd[18])<<16 | uint32(eocd[19])<<24)
	return cdOffset, cdSize, nil
}

// parseZip64EOCD handles ZIP64 format to get real CD offset/size.
func parseZip64EOCD(buf []byte, eocdPos, bufferStartOffset int) (cdOffset, cdSize int, err error) {
	// Look for ZIP64 EOCD locator (20 bytes before EOCD)
	locPos := eocdPos - 20
	if locPos < 0 || !bytes.Equal(buf[locPos:locPos+4], zip64LocSig) {
		return 0, 0, fmt.Errorf("ZIP64 EOCD locator not found")
	}

	// Parse ZIP64 EOCD locator to find ZIP64 EOCD offset
	zip64EOCDOffset := int(uint64(buf[locPos+8]) | uint64(buf[locPos+9])<<8 |
		uint64(buf[locPos+10])<<16 | uint64(buf[locPos+11])<<24 |
		uint64(buf[locPos+12])<<32 | uint64(buf[locPos+13])<<40 |
		uint64(buf[locPos+14])<<48 | uint64(buf[locPos+15])<<56)

	localOffset := zip64EOCDOffset - bufferStartOffset
	if localOffset < 0 || localOffset+56 > len(buf) {
		return 0, 0, fmt.Errorf("ZIP64 EOCD not in buffer")
	}

	zip64EOCD := buf[localOffset:]
	if !bytes.Equal(zip64EOCD[0:4], zip64EOCDSig) {
		return 0, 0, fmt.Errorf("ZIP64 EOCD signature mismatch")
	}

	cdSize = int(uint64(zip64EOCD[40]) | uint64(zip64EOCD[41])<<8 |
		uint64(zip64EOCD[42])<<16 | uint64(zip64EOCD[43])<<24 |
		uint64(zip64EOCD[44])<<32 | uint64(zip64EOCD[45])<<40 |
		uint64(zip64EOCD[46])<<48 | uint64(zip64EOCD[47])<<56)
	cdOffset = int(uint64(zip64EOCD[48]) | uint64(zip64EOCD[49])<<8 |
		uint64(zip64EOCD[50])<<16 | uint64(zip64EOCD[51])<<24 |
		uint64(zip64EOCD[52])<<32 | uint64(zip64EOCD[53])<<40 |
		uint64(zip64EOCD[54])<<48 | uint64(zip64EOCD[55])<<56)

	return cdOffset, cdSize, nil
}

// recoverCDIndex attempts to recover CD entries from a truncated ZIP
// by scanning backwards from EOF to find CD signatures.
func recoverCDIndex[T any](src *stream.ChunkedFile[T]) (ZipIndex, error) {
	const chunkSize = 512 << 10 // 512KB chunks for scanning
	const maxSearch = 64 << 20  // Search up to 64MB from EOF
	const gapLimit = 256 << 10  // Stop if gap > 256KB with no CDs

	searchLimit := min(maxSearch, src.Size())
	earliestCDOffset := src.Size() // Track earliest CD we've found

	// Probe backwards to find where CDs start
	for probeEnd := src.Size(); probeEnd > src.Size()-searchLimit; {
		probeStart := max(0, probeEnd-chunkSize)
		probeLen := probeEnd - probeStart

		chunk, err := readRange(src, probeStart, probeLen)
		if err != nil {
			return nil, fmt.Errorf("failed to read chunk during recovery: %w", err)
		}

		// Find CD signatures in this chunk
		foundInChunk := false
		for i := 0; i <= len(chunk)-4; i++ {
			if bytes.Equal(chunk[i:i+4], cdSig) {
				foundInChunk = true
				absOffset := probeStart + i
				if absOffset < earliestCDOffset {
					earliestCDOffset = absOffset
				}
			}
		}

		if !foundInChunk && earliestCDOffset < src.Size() {
			// We found CDs before but not in this chunk - we've hit the start
			break
		}

		if !foundInChunk && src.Size()-probeStart > gapLimit {
			// Large gap with no CDs and we haven't found any yet
			return nil, fmt.Errorf("no Central Directory entries found in first %d bytes from EOF", gapLimit)
		}

		probeEnd = probeStart
	}

	if earliestCDOffset >= src.Size() {
		return nil, fmt.Errorf("no Central Directory entries found")
	}

	// Read from earliest CD to EOF and parse
	cdData, err := readRange(src, earliestCDOffset, src.Size()-earliestCDOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to read recovered CD region: %w", err)
	}

	index, err := parseCDEntries(cdData)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "WARNING: Recovered %d files from truncated ZIP\n", len(index))
	return index, nil
}

// parseCDEntries parses Central Directory entries from a buffer.
func parseCDEntries(cdData []byte) (ZipIndex, error) {
	var index []ZippedFile
	pos := 0

	for pos < len(cdData)-46 {
		if !bytes.Equal(cdData[pos:pos+4], cdSig) {
			break
		}

		entry := ZippedFile{}
		entry.compression = uint16(cdData[pos+10]) | uint16(cdData[pos+11])<<8
		entry.src.length = int(uint32(cdData[pos+20]) | uint32(cdData[pos+21])<<8 |
			uint32(cdData[pos+22])<<16 | uint32(cdData[pos+23])<<24)
		entry.Size = util.BytesSize(uint32(cdData[pos+24]) | uint32(cdData[pos+25])<<8 |
			uint32(cdData[pos+26])<<16 | uint32(cdData[pos+27])<<24)
		fileNameLength := uint16(cdData[pos+28]) | uint16(cdData[pos+29])<<8

		extraFieldLength := uint16(cdData[pos+30]) | uint16(cdData[pos+31])<<8
		commentLen := uint16(cdData[pos+32]) | uint16(cdData[pos+33])<<8
		entry.src.offset = int(uint32(cdData[pos+42]) | uint32(cdData[pos+43])<<8 |
			uint32(cdData[pos+44])<<16 | uint32(cdData[pos+45])<<24)

		// Read filename
		nameStart := pos + 46
		nameEnd := nameStart + int(fileNameLength)
		if nameEnd > len(cdData) {
			break
		}
		entry.Name = string(cdData[nameStart:nameEnd])

		// Handle ZIP64 extra field
		extraStart := nameEnd
		extraEnd := extraStart + int(extraFieldLength)
		if extraEnd <= len(cdData) && extraFieldLength > 0 {
			extra := cdData[extraStart:extraEnd]
			// Parse ZIP64 extended info (ID 0x0001)
			for i := 0; i+4 <= len(extra); {
				id := uint16(extra[i]) | uint16(extra[i+1])<<8
				size := uint16(extra[i+2]) | uint16(extra[i+3])<<8
				if id == 0x0001 && i+4+int(size) <= len(extra) {
					z64 := extra[i+4 : i+4+int(size)]
					offset := 0
					if entry.Size == 0xFFFFFFFF && offset+8 <= len(z64) {
						entry.Size = util.BytesSize(uint64(z64[offset]) | uint64(z64[offset+1])<<8 |
							uint64(z64[offset+2])<<16 | uint64(z64[offset+3])<<24 |
							uint64(z64[offset+4])<<32 | uint64(z64[offset+5])<<40 |
							uint64(z64[offset+6])<<48 | uint64(z64[offset+7])<<56)
						offset += 8
					}
					if entry.src.length == 0xFFFFFFFF && offset+8 <= len(z64) {
						entry.src.length = int(uint64(z64[offset]) | uint64(z64[offset+1])<<8 |
							uint64(z64[offset+2])<<16 | uint64(z64[offset+3])<<24 |
							uint64(z64[offset+4])<<32 | uint64(z64[offset+5])<<40 |
							uint64(z64[offset+6])<<48 | uint64(z64[offset+7])<<56)
						offset += 8
					}
					if entry.src.offset == 0xFFFFFFFF && offset+8 <= len(z64) {
						entry.src.offset = int(uint64(z64[offset]) | uint64(z64[offset+1])<<8 |
							uint64(z64[offset+2])<<16 | uint64(z64[offset+3])<<24 |
							uint64(z64[offset+4])<<32 | uint64(z64[offset+5])<<40 |
							uint64(z64[offset+6])<<48 | uint64(z64[offset+7])<<56)
					}
					break
				}
				i += 4 + int(size)
			}
		}

		index = append(index, entry)
		pos = extraEnd + int(commentLen)
	}

	return index, nil
}

// Filtered returns entries matching any include pattern and not matching any
// exclude pattern. If includes is empty, all non-excluded entries are returned.
func (i ZipIndex) Filtered(includes, excludes []string) []ZippedFile {
	var result []ZippedFile

	matches := func(pattern, name string) bool {
		matched, _ := filepath.Match(pattern, name)
		return matched
	}

	for _, entry := range i {
		// Skip directories
		if strings.HasSuffix(entry.Name, "/") {
			continue
		}

		// Check excludes first
		excluded := false
		for _, ex := range excludes {
			if matches(ex, entry.Name) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		// Check filters (if any specified)
		if len(includes) > 0 {
			matched := false
			for _, f := range includes {
				if matches(f, entry.Name) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		result = append(result, entry)
	}

	return result
}

// StripCommonPrefix removes a common folder prefix from all entries if every
// entry shares the same top-level directory. Returns the prefix that was stripped.
func (idx ZipIndex) StripCommonPrefix() string {
	if len(idx) == 0 {
		return ""
	}

	// Find common prefix by checking first path component
	var commonPrefix string
	for i, entry := range idx {
		// Skip directory entries
		if strings.HasSuffix(entry.Name, "/") {
			continue
		}

		parts := strings.SplitN(entry.Name, "/", 2)
		if len(parts) < 2 {
			// File at root level - no common prefix possible
			return ""
		}

		firstDir := parts[0] + "/"
		if i == 0 || commonPrefix == "" {
			commonPrefix = firstDir
		} else if firstDir != commonPrefix {
			// Different prefix - no common prefix
			return ""
		}
	}

	if commonPrefix == "" {
		return ""
	}

	// Strip the prefix from all entries
	for i := range idx {
		idx[i].Name = strings.TrimPrefix(idx[i].Name, commonPrefix)
	}

	return commonPrefix
}
