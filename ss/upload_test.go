package ss

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// mockDropzoneServer creates a test server that simulates the SendSafely dropzone API.
type mockDropzoneServer struct {
	*httptest.Server
	packages     map[string]*mockUploadPackage
	uploadedData map[string][]byte // fileID:part -> encrypted data
	dropzoneID   string
}

type mockUploadPackage struct {
	packageID    string
	packageCode  string
	serverSecret string
	keyCode      string
	files        map[string]*mockUploadFile
	finalized    bool
}

type mockUploadFile struct {
	fileID   string
	filename string
	parts    int
	filesize int64
	complete bool
}

func newMockDropzoneServer() *mockDropzoneServer {
	m := &mockDropzoneServer{
		packages:     make(map[string]*mockUploadPackage),
		uploadedData: make(map[string][]byte),
		dropzoneID:   "test-dropzone-id",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/drop-zone/v2.0/package/", m.handlePackage)
	mux.HandleFunc("/upload/", m.handleUpload)
	mux.HandleFunc("/auth/json/", m.handleSubmit)

	m.Server = httptest.NewServer(mux)
	return m
}

func (m *mockDropzoneServer) handlePackage(w http.ResponseWriter, r *http.Request) {
	// Validate dropzone ID
	if r.Header.Get("ss-api-key") != m.dropzoneID {
		json.NewEncoder(w).Encode(map[string]string{
			"response": "AUTHENTICATION_FAILED",
		})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/drop-zone/v2.0/package/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	// PUT /drop-zone/v2.0/package/ - create package
	if r.Method == "PUT" && len(parts) == 1 && parts[0] == "" {
		pkg := &mockUploadPackage{
			packageID:    "pkg-" + randomString(8),
			packageCode:  "CODE-" + randomString(8),
			serverSecret: "secret-" + randomString(16),
			files:        make(map[string]*mockUploadFile),
		}
		m.packages[pkg.packageID] = pkg

		json.NewEncoder(w).Encode(map[string]interface{}{
			"response":     "SUCCESS",
			"packageId":    pkg.packageID,
			"packageCode":  pkg.packageCode,
			"serverSecret": pkg.serverSecret,
		})
		return
	}

	// Parse packageID from path
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}
	packageID := parts[0]
	pkg, ok := m.packages[packageID]
	if !ok {
		json.NewEncoder(w).Encode(map[string]string{
			"response": "PACKAGE_NOT_FOUND",
		})
		return
	}

	// PUT /drop-zone/v2.0/package/{pkgId}/file/ - create file
	if r.Method == "PUT" && len(parts) >= 2 && parts[1] == "file" && (len(parts) == 2 || parts[2] == "") {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)

		file := &mockUploadFile{
			fileID:   "file-" + randomString(8),
			filename: body["filename"].(string),
			parts:    int(body["parts"].(float64)),
			filesize: int64(body["filesize"].(float64)),
		}
		pkg.files[file.fileID] = file

		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "SUCCESS",
			"fileId":   file.fileID,
		})
		return
	}

	// POST /drop-zone/v2.0/package/{pkgId}/file/{fileId}/upload-urls/ - get upload URLs
	if r.Method == "POST" && len(parts) >= 4 && parts[1] == "file" && parts[3] == "upload-urls" {
		fileID := parts[2]
		file, ok := pkg.files[fileID]
		if !ok {
			json.NewEncoder(w).Encode(map[string]string{
				"response": "FILE_NOT_FOUND",
			})
			return
		}

		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		part := int(body["part"].(float64))

		if part < 1 || part > file.parts {
			json.NewEncoder(w).Encode(map[string]string{
				"response": "INVALID_PART",
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "SUCCESS",
			"uploadUrls": []map[string]interface{}{
				{
					"part": part,
					"url":  m.Server.URL + "/upload/" + packageID + "/" + fileID + "/" + itoa(part),
				},
			},
		})
		return
	}

	// POST /drop-zone/v2.0/package/{pkgId}/file/{fileId}/upload-complete/ - mark complete
	if r.Method == "POST" && len(parts) >= 4 && parts[1] == "file" && parts[3] == "upload-complete" {
		fileID := parts[2]
		file, ok := pkg.files[fileID]
		if !ok {
			json.NewEncoder(w).Encode(map[string]string{
				"response": "FILE_NOT_FOUND",
			})
			return
		}

		file.complete = true
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "SUCCESS",
		})
		return
	}

	// POST /drop-zone/v2.0/package/{pkgId}/finalize/ - finalize package
	if r.Method == "POST" && len(parts) >= 2 && parts[1] == "finalize" {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)

		pkg.keyCode = body["keyCode"].(string)
		pkg.finalized = true

		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "SUCCESS",
			"link":     m.Server.URL + "/receive/?packageCode=" + pkg.packageCode + "#keyCode=" + pkg.keyCode,
		})
		return
	}

	http.NotFound(w, r)
}

func (m *mockDropzoneServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	// PUT /upload/{packageId}/{fileId}/{part}
	path := strings.TrimPrefix(r.URL.Path, "/upload/")
	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}

	key := parts[1] + ":" + parts[2]
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m.uploadedData[key] = data
	w.WriteHeader(http.StatusOK)
}

func (m *mockDropzoneServer) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("ss-api-key") != m.dropzoneID {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"response": "SUCCESS",
	})
}

func randomString(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	f, _ := os.Open("/dev/urandom")
	f.Read(b)
	f.Close()
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestComputeParts tests part calculation for various file sizes.
func TestComputeParts(t *testing.T) {
	tests := []struct {
		size     int64
		expected int
	}{
		{0, 1},                                   // Empty file
		{100, 1},                                 // Small file
		{FirstSegmentSize, 1},                    // Exactly first segment
		{FirstSegmentSize + 1, 2},                // Just over first segment
		{FirstSegmentSize + SegmentSize, 2},      // First + one full segment
		{FirstSegmentSize + SegmentSize + 1, 3},  // Need third part
		{FirstSegmentSize + SegmentSize*2, 3},    // First + two full segments
		{FirstSegmentSize + SegmentSize*10, 11},  // Large file
	}

	for _, tc := range tests {
		got := computeParts(tc.size)
		if got != tc.expected {
			t.Errorf("computeParts(%d) = %d, want %d", tc.size, got, tc.expected)
		}
	}
}

// TestPartBounds tests part boundary calculation.
func TestPartBounds(t *testing.T) {
	tests := []struct {
		part     int
		fileSize int64
		offset   int64
		length   int64
	}{
		// Small file (one part)
		{1, 1000, 0, 1000},

		// File exactly FirstSegmentSize
		{1, FirstSegmentSize, 0, FirstSegmentSize},

		// File with two parts
		{1, FirstSegmentSize + 1000, 0, FirstSegmentSize},
		{2, FirstSegmentSize + 1000, FirstSegmentSize, 1000},

		// File with multiple parts
		{1, int64(FirstSegmentSize + SegmentSize*2 + 500), 0, FirstSegmentSize},
		{2, int64(FirstSegmentSize + SegmentSize*2 + 500), FirstSegmentSize, SegmentSize},
		{3, int64(FirstSegmentSize + SegmentSize*2 + 500), int64(FirstSegmentSize + SegmentSize), SegmentSize},
		{4, int64(FirstSegmentSize + SegmentSize*2 + 500), int64(FirstSegmentSize + SegmentSize*2), 500},
	}

	for _, tc := range tests {
		offset, length := partBounds(tc.part, tc.fileSize)
		if offset != tc.offset || length != tc.length {
			t.Errorf("partBounds(%d, %d) = (%d, %d), want (%d, %d)",
				tc.part, tc.fileSize, offset, length, tc.offset, tc.length)
		}
	}
}

// TestEncryptDecryptRoundTrip tests encryption/decryption round-trip.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	original := []byte("Hello, World! This is test data for encryption.")
	passphrase := "server-secret-123keycode-456"

	encrypted, err := EncryptPGP(original, passphrase)
	if err != nil {
		t.Fatalf("EncryptPGP failed: %v", err)
	}

	// Decrypt using openpgp
	md, err := openpgp.ReadMessage(bytes.NewReader(encrypted), nil, func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		return []byte(passphrase), nil
	}, nil)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	decrypted, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if !bytes.Equal(decrypted, original) {
		t.Errorf("Decrypted data doesn't match original.\nGot: %s\nWant: %s", decrypted, original)
	}
}

// TestDropzoneClient_CreatePackage tests creating a new package.
func TestDropzoneClient_CreatePackage(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, server.dropzoneID)
	pkg, err := client.CreatePackage()
	if err != nil {
		t.Fatalf("CreatePackage failed: %v", err)
	}

	if pkg.PackageID == "" {
		t.Error("PackageID should not be empty")
	}
	if pkg.PackageCode == "" {
		t.Error("PackageCode should not be empty")
	}
	if pkg.ServerSecret == "" {
		t.Error("ServerSecret should not be empty")
	}
	if len(pkg.KeyCode) != 64 {
		t.Errorf("KeyCode should be 64 chars, got %d", len(pkg.KeyCode))
	}
}

// TestUploadPackage_AddFile tests uploading a file.
func TestUploadPackage_AddFile(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, server.dropzoneID)
	pkg, err := client.CreatePackage()
	if err != nil {
		t.Fatalf("CreatePackage failed: %v", err)
	}

	// Create test data (small enough for one part)
	testData := []byte("This is test file content for upload testing.")
	reader := bytes.NewReader(testData)

	var progressCalls int
	fileID, err := pkg.AddFile("test.txt", reader, int64(len(testData)), func(p UploadProgress) {
		progressCalls++
		if p.FileName != "test.txt" {
			t.Errorf("Progress filename = %s, want test.txt", p.FileName)
		}
	})
	if err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	if fileID == "" {
		t.Error("FileID should not be empty")
	}

	if progressCalls != 1 {
		t.Errorf("Progress called %d times, want 1", progressCalls)
	}

	// Verify data was uploaded and can be decrypted
	uploadedKey := fileID + ":1"
	encrypted, ok := server.uploadedData[uploadedKey]
	if !ok {
		t.Fatal("Uploaded data not found")
	}

	// Decrypt and verify
	passphrase := pkg.ServerSecret + pkg.KeyCode
	md, err := openpgp.ReadMessage(bytes.NewReader(encrypted), nil, func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		return []byte(passphrase), nil
	}, nil)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	decrypted, _ := io.ReadAll(md.UnverifiedBody)
	if !bytes.Equal(decrypted, testData) {
		t.Errorf("Decrypted data doesn't match.\nGot: %s\nWant: %s", decrypted, testData)
	}
}

// TestUploadPackage_AddFile_MultiPart tests uploading a file with multiple parts.
func TestUploadPackage_AddFile_MultiPart(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, server.dropzoneID)
	pkg, err := client.CreatePackage()
	if err != nil {
		t.Fatalf("CreatePackage failed: %v", err)
	}

	// Create test data larger than FirstSegmentSize to trigger multiple parts
	testData := bytes.Repeat([]byte("X"), FirstSegmentSize+1000)
	reader := bytes.NewReader(testData)

	var progressCalls []UploadProgress
	fileID, err := pkg.AddFile("large.bin", reader, int64(len(testData)), func(p UploadProgress) {
		progressCalls = append(progressCalls, p)
	})
	if err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	if len(progressCalls) != 2 {
		t.Errorf("Progress called %d times, want 2", len(progressCalls))
	}

	// Verify both parts were uploaded
	for part := 1; part <= 2; part++ {
		key := fileID + ":" + itoa(part)
		if _, ok := server.uploadedData[key]; !ok {
			t.Errorf("Part %d not uploaded", part)
		}
	}

	// Verify first part decrypts correctly
	passphrase := pkg.ServerSecret + pkg.KeyCode
	md, _ := openpgp.ReadMessage(bytes.NewReader(server.uploadedData[fileID+":1"]), nil, func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		return []byte(passphrase), nil
	}, nil)
	decrypted, _ := io.ReadAll(md.UnverifiedBody)
	if len(decrypted) != FirstSegmentSize {
		t.Errorf("First part size = %d, want %d", len(decrypted), FirstSegmentSize)
	}

	// Verify second part decrypts correctly
	md2, _ := openpgp.ReadMessage(bytes.NewReader(server.uploadedData[fileID+":2"]), nil, func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		return []byte(passphrase), nil
	}, nil)
	decrypted2, _ := io.ReadAll(md2.UnverifiedBody)
	if len(decrypted2) != 1000 {
		t.Errorf("Second part size = %d, want 1000", len(decrypted2))
	}
}

// TestUploadPackage_AddFilePath tests uploading from a file path.
func TestUploadPackage_AddFilePath(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, server.dropzoneID)
	pkg, err := client.CreatePackage()
	if err != nil {
		t.Fatalf("CreatePackage failed: %v", err)
	}

	// Create a temp file
	tmpDir, err := os.MkdirTemp("", "upload-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testContent := []byte("Test file content from disk.")
	testPath := filepath.Join(tmpDir, "upload-test.txt")
	if err := os.WriteFile(testPath, testContent, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	fileID, err := pkg.AddFilePath(testPath, nil)
	if err != nil {
		t.Fatalf("AddFilePath failed: %v", err)
	}

	if fileID == "" {
		t.Error("FileID should not be empty")
	}
}

// TestUploadPackage_Finalize tests finalizing a package.
func TestUploadPackage_Finalize(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, server.dropzoneID)
	pkg, err := client.CreatePackage()
	if err != nil {
		t.Fatalf("CreatePackage failed: %v", err)
	}

	// Add a file first
	testData := []byte("Test content")
	_, err = pkg.AddFile("test.txt", bytes.NewReader(testData), int64(len(testData)), nil)
	if err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	// Finalize
	link, err := pkg.Finalize("test@example.com")
	if err != nil {
		t.Fatalf("Finalize failed: %v", err)
	}

	if link == "" {
		t.Error("Link should not be empty")
	}

	if !strings.Contains(link, "packageCode=") {
		t.Errorf("Link should contain packageCode: %s", link)
	}

	if !strings.Contains(link, "keyCode=") {
		t.Errorf("Link should contain keyCode: %s", link)
	}

	// Verify package was finalized on server
	mockPkg := server.packages[pkg.PackageID]
	if !mockPkg.finalized {
		t.Error("Package should be marked as finalized")
	}
}

// TestUploadPackage_SubmitHostedDropzone tests dropzone submission.
func TestUploadPackage_SubmitHostedDropzone(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, server.dropzoneID)
	pkg, err := client.CreatePackage()
	if err != nil {
		t.Fatalf("CreatePackage failed: %v", err)
	}

	err = pkg.SubmitHostedDropzone("test@example.com", "TICKET-123")
	if err != nil {
		t.Fatalf("SubmitHostedDropzone failed: %v", err)
	}
}

// TestDropzoneClient_InvalidDropzoneID tests authentication failure.
func TestDropzoneClient_InvalidDropzoneID(t *testing.T) {
	server := newMockDropzoneServer()
	defer server.Close()

	client := NewDropzoneClient(server.Server.URL, "invalid-dropzone-id")
	_, err := client.CreatePackage()
	if err == nil {
		t.Fatal("Expected error for invalid dropzone ID")
	}
	if !strings.Contains(err.Error(), "AUTHENTICATION_FAILED") {
		t.Errorf("Expected AUTHENTICATION_FAILED error, got: %v", err)
	}
}
