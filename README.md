# Go SendSafely Library and CLI Tools

A collection of tools for downloading and listing files from SendSafely, along with uploading files to dropzones and extracting individual files from ZIP archives stored in SendSafely packages.

## CLI Tools

* `ssget` - Download or list files from a SendSafely package i.e. 'wget for SendSafely'.
* `ssdrop` - Upload files to a SendSafely dropzone.
* `ssunzip` - Download and extract all or only some files stored in a ZIP archive in a SendSafely package.

### ssget

A simple file downloader that can download or list files from SendSafely packages.

```bash
ssget <sendsafely-url> [file-patterns...]
```

**Example:**
```bash
# Download all files from a SendSafely package
ssget "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789"

# List files from a package rather than downloading them
ssget -l "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789"

# Download only ".zip" files
ssget "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789" "*.zip"

```

**Flags:**
- `-o, --out` - Output directory (default: current directory)
- `-l, --list` - List files in the package rather than download them
- `--no-keyring` - Don't use system keychain for credentials
- `--forget-keyring` - Remove saved credentials from system keychain

**Features:**
- Resumes partially downloaded files automatically
- Skips files that already exist with matching size
- Internally pipelines concurrent chunk fetching and decryption to increase throughput

### ssdrop

Upload files to a SendSafely dropzone.

```bash
ssdrop --url=<dropzone-url> --dropzone-id=<id> --email=<email> FILE [FILE...]
```

**Example:**
```bash
# Upload a single file
ssdrop --url="https://dropzone.example.com" \
       --dropzone-id="abc123" \
       --email="sender@example.com" \
       report.pdf

# Upload multiple files
ssdrop --url="https://dropzone.example.com" \
       --dropzone-id="abc123" \
       --email="sender@example.com" \
       file1.txt file2.zip
```

**Flags:**
- `--url` - Dropzone URL (required)
- `--dropzone-id` - Dropzone ID (required)
- `--email` - Sender email address (required)
- `--name` - Submitter name or ticket ID (optional, for integrations)

**Features:**
- No API credentials required — only dropzone URL and ID
- Client-side encryption before upload
- Returns a secure link for sharing with recipients
- Internally pipelines concurrent chunk encryption and uploads

### ssunzip

Extract some or all files from a ZIP archive stored remotely in a SendSafely package, fetching the minimum fraction of the remote file for the selected files when choosing individual files.

```bash
ssunzip <sendsafely-url> [file-patterns...]
```

**Example:**
```bash
# Extract all files
ssunzip "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789"

# Extract only JSON files
ssunzip "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789" "*.json"

# Extract with exclusions
ssunzip "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789" -x "nodes/*/*.log"

# List contents without extracting
ssunzip -l "https://sendsafely.example.com/receive/?packageCode=ABC123#keyCode=xyz789"
```

**Flags:**
- `-o, --out` - Output directory (default: ZIP filename without extension)
- `-z, --zip-file` - Name of ZIP file in package (auto-detected if only one)
- `-l, --list` - List files without extracting
- `-x, --exclude` - Exclude files matching pattern (can be repeated and combined with inclusion filters)
- `--no-keyring` - Don't use system keychain for credentials
- `--forget-keyring` - Remove saved credentials from system keychain

**Features:**
- Skips files that already exist with matching size, allowing resumption of interrupted extractions
- Supports optional glob-pattern filtering and exclusion for selective extraction
  - Fetches and extracts only the selected files directly from the remote ZIP
- Automatically strips top-level folder when archive contains a single folder
- Handles (slightly) truncated archives, decoding available index entries
  - *Caveat:* Only recovers files whose central directory entries are intact (i.e. where truncation of the archive was minimal or limited to few KB). For comprehensive recovery, download the full archive via `ssget` and use a local ZIP recovery tool.
- Internally pipelines concurrent chunk fetching, decryption and file extraction

## Installation

**Install Pre-built binaries:**

Pre-built binaries for macOS and Linux are available on [GitHub Releases](https://github.com/dt/gosendsafely/releases).

Download the applicable binary to a folder in `PATH`, or use the automatic install script to do so:

```bash
curl -fsSL https://raw.githubusercontent.com/dt/gosendsafely/main/install.sh | sh
```

To specify an install directory:
```bash
curl -fsSL https://raw.githubusercontent.com/dt/gosendsafely/main/install.sh | INSTALL_DIR=~/.local/bin sh
```

The script auto-selects from `~/bin`, `~/.local/bin`, or `/usr/local/bin` based on which is first in PATH. Falls back to current directory if none are available.

**Build and install from source:**
```bash
go install github.com/dt/gosendsafely/cmd/...@latest
```
Requires Go 1.23+ and a 64-bit system.

## Credentials

API credentials are loaded from (in order):

1. **Environment variables:** `SS_API_KEY` and `SS_API_SECRET`
2. **System keychain:** Securely stored credentials from previous sessions
3. **Interactive prompt:** If running in a terminal, prompts for credentials (with option to save to keychain)

## Design

The implementation uses a streaming architecture optimized for large encrypted archives, organized into focused packages:

**Package Structure:**
- `stream/` - Chunked file abstraction for seekable streaming of chunked content
- `ziputil/` - ZIP archive processing (index decoding, selective extraction, recovery) for archives stored in chunked files.
- `util/` - Shared utilities (byte size formatting, concurrency limits)
- `sendsafely/` - SendSafely API client (package downloads, dropzone uploads, authentication)

**Chunked File Abstraction (`stream/`):** SendSafely files are stored as PGP-encrypted chunks. The `ChunkedFile` type presents a seekable byte stream backed by fetching and decrypting remote chunks on demand.

**Parallel Pipeline:** Downloads run as a producer-consumer pipeline:
- A pool of fetcher goroutines downloads and decrypts chunks concurrently (up to 16 parallel requests)
- Reader goroutines consume chunk data, blocking as needed until notified by a fetcher goroutine when a chunk becomes available
- For ZIP extraction, parallel extractors decompress and write files to disk

**Minimally Blocking Reads:** A reader for any given span of a file will read from one or more chunks. Needed chunks for all opened readers are fetched and decrypted concurrently. As a reader reads, when it advances into a chunk, it blocks on a ready channel until the chunk's content is available. This allows extraction to proceed as fast as chunks arrive without explicit coordination.

**Reference-Counted Chunk Management:** Each reader references the chunks it needs. Chunks are fetched only when referenced, and content is freed immediately when the last reader referencing a chunk finishes with that chunk. This prevents memory exhaustion when processing large archives—only the working set of chunks is held in memory.

**Streaming Selective ZIP Processing (`ziputil/`):** For `ssunzip`, the ZIP central directory (CD) is read first from the end of the file to decode the index. Each selected file's compressed data is then read directly from its offset in the archive, decompressed, and written to disk—all in parallel with chunk downloading. This works because the CD provides file offsets, enabling selective access without downloading the entire archive.

**Truncated ZIP Recovery:** If the file is slightly truncated (missing the EOCD marker and some suffix of the CD), recovery mode scans backward from EOF to find any CD entries that remain intact. Files with valid CD entries can still be extracted; entries in the truncated portion are lost (even though their content is in the data portion of the ZIP).

A more comprehensive recovery approach could scan the data portion of the archive for local file headers rather than depending on CD entries. This would recover more files when the CD is truncated, but requires fetching the entire archive — at which point you might as well download it whole and use a standard ZIP recovery tool locally.

## License

Apache 2.0 — see [LICENSE](LICENSE) for details.
