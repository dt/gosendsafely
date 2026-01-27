package ss

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Upload constants matching SendSafely protocol.
const (
	SegmentSize      = 2621440 // 2.5MB - size of parts 2+
	FirstSegmentSize = 655360  // 640KB - size of part 1 (SegmentSize/4)
)

// DropzoneClient handles anonymous dropzone uploads.
type DropzoneClient struct {
	baseURL    string
	dropzoneID string
	http       *http.Client
}

// NewDropzoneClient creates a client for a specific dropzone.
func NewDropzoneClient(baseURL, dropzoneID string) *DropzoneClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16

	return &DropzoneClient{
		baseURL:    baseURL,
		dropzoneID: dropzoneID,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// UploadPackage represents an in-progress upload.
type UploadPackage struct {
	client       *DropzoneClient
	PackageID    string
	PackageCode  string
	ServerSecret string
	KeyCode      string
}

// UploadProgress reports upload status.
type UploadProgress struct {
	FileName string
	FileID   string
	Part     int
	Parts    int
	Bytes    int64
	Total    int64
}

// CreatePackage creates a new dropzone package for uploading.
func (c *DropzoneClient) CreatePackage() (*UploadPackage, error) {
	urlPath := "/drop-zone/v2.0/package/"

	resp, err := c.doRequest("PUT", urlPath, nil)
	if err != nil {
		return nil, fmt.Errorf("create package: %w", err)
	}

	var pkgResp struct {
		Response     string `json:"response"`
		Message      string `json:"message"`
		PackageID    string `json:"packageId"`
		PackageCode  string `json:"packageCode"`
		ServerSecret string `json:"serverSecret"`
	}
	if err := json.Unmarshal(resp, &pkgResp); err != nil {
		return nil, fmt.Errorf("parse create package response: %w", err)
	}

	if pkgResp.Response != "SUCCESS" {
		return nil, fmt.Errorf("create package failed: %s - %s", pkgResp.Response, pkgResp.Message)
	}

	// Generate a random keyCode (32 hex chars)
	keyCode, err := generateKeyCode()
	if err != nil {
		return nil, fmt.Errorf("generate keycode: %w", err)
	}

	return &UploadPackage{
		client:       c,
		PackageID:    pkgResp.PackageID,
		PackageCode:  pkgResp.PackageCode,
		ServerSecret: pkgResp.ServerSecret,
		KeyCode:      keyCode,
	}, nil
}

func generateKeyCode() (string, error) {
	b := make([]byte, 32)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.ReadFull(f, b); err != nil {
		return "", err
	}
	const hexChars = "0123456789abcdef"
	result := make([]byte, 64)
	for i, v := range b {
		result[i*2] = hexChars[v>>4]
		result[i*2+1] = hexChars[v&0x0f]
	}
	return string(result), nil
}

// AddFile uploads a file to the package.
// Accepts io.ReaderAt + size for flexibility (works with os.File, bytes.Reader, etc.)
func (p *UploadPackage) AddFile(name string, r io.ReaderAt, size int64, progress func(UploadProgress)) (fileID string, err error) {
	parts := computeParts(size)

	// Create file entry
	fileID, err = p.createFile(name, parts, size)
	if err != nil {
		return "", err
	}

	// Upload each part
	passphrase := p.ServerSecret + p.KeyCode
	var uploadedBytes int64

	for part := 1; part <= parts; part++ {
		offset, length := partBounds(part, size)

		// Read the part data
		data := make([]byte, length)
		if _, err := r.ReadAt(data, offset); err != nil && err != io.EOF {
			return "", fmt.Errorf("read part %d: %w", part, err)
		}

		// Encrypt the part
		encrypted, err := EncryptPGP(data, passphrase)
		if err != nil {
			return "", fmt.Errorf("encrypt part %d: %w", part, err)
		}

		// Upload the part
		if err := p.uploadPart(fileID, part, encrypted); err != nil {
			return "", fmt.Errorf("upload part %d: %w", part, err)
		}

		uploadedBytes += length
		if progress != nil {
			progress(UploadProgress{
				FileName: name,
				FileID:   fileID,
				Part:     part,
				Parts:    parts,
				Bytes:    uploadedBytes,
				Total:    size,
			})
		}
	}

	// Mark upload complete
	if err := p.markUploadComplete(fileID); err != nil {
		return "", fmt.Errorf("mark complete: %w", err)
	}

	return fileID, nil
}

// AddFilePath is a convenience wrapper that opens a file path.
func (p *UploadPackage) AddFilePath(path string, progress func(UploadProgress)) (fileID string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	return p.AddFile(info.Name(), f, info.Size(), progress)
}

// Finalize completes the package and returns the secure link.
func (p *UploadPackage) Finalize(email string) (secureLink string, err error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/finalize/", p.PackageID)

	body := map[string]interface{}{
		"keyCode": p.KeyCode,
		"email":   email,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("POST", urlPath, bodyBytes)
	if err != nil {
		return "", fmt.Errorf("finalize: %w", err)
	}

	var finResp struct {
		Response string `json:"response"`
		Message  string `json:"message"`
		Link     string `json:"link"`
	}
	if err := json.Unmarshal(resp, &finResp); err != nil {
		return "", fmt.Errorf("parse finalize response: %w", err)
	}

	if finResp.Response != "SUCCESS" {
		return "", fmt.Errorf("finalize failed: %s - %s", finResp.Response, finResp.Message)
	}

	return finResp.Link, nil
}

// SubmitHostedDropzone submits to the dropzone webhook (for Zendesk integration etc.)
func (p *UploadPackage) SubmitHostedDropzone(email, label string) error {
	urlPath := "/auth/json/"

	formData := url.Values{}
	formData.Set("email", email)
	formData.Set("label", label)
	formData.Set("packageCode", p.PackageCode)
	formData.Set("keyCode", p.KeyCode)

	req, err := http.NewRequest("POST", p.client.baseURL+urlPath, bytes.NewBufferString(formData.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("ss-api-key", p.client.dropzoneID)

	resp, err := p.client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dropzone submission failed (%d): %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}

	return nil
}

// doRequest performs an HTTP request to the dropzone API.
func (c *DropzoneClient) doRequest(method, urlPath string, body []byte) ([]byte, error) {
	fullURL := c.baseURL + urlPath
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("ss-api-key", c.dropzoneID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))]))
	}

	return respBody, nil
}

// computeParts calculates part count using SendSafely's algorithm.
func computeParts(size int64) int {
	if size <= FirstSegmentSize {
		return 1
	}
	return 1 + int(math.Ceil(float64(size-FirstSegmentSize)/float64(SegmentSize)))
}

// partBounds returns (offset, length) for a given part number (1-indexed).
func partBounds(part int, fileSize int64) (offset, length int64) {
	if part == 1 {
		return 0, min(int64(FirstSegmentSize), fileSize)
	}
	offset = int64(FirstSegmentSize) + int64(part-2)*int64(SegmentSize)
	length = min(int64(SegmentSize), fileSize-offset)
	return offset, length
}

// createFile creates a file entry in the package.
func (p *UploadPackage) createFile(name string, parts int, size int64) (string, error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/", p.PackageID)

	body := map[string]interface{}{
		"filename": name,
		"parts":    parts,
		"filesize": size,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("PUT", urlPath, bodyBytes)
	if err != nil {
		return "", err
	}

	var fileResp struct {
		Response string `json:"response"`
		Message  string `json:"message"`
		FileID   string `json:"fileId"`
	}
	if err := json.Unmarshal(resp, &fileResp); err != nil {
		return "", err
	}

	if fileResp.Response != "SUCCESS" {
		return "", fmt.Errorf("create file failed: %s - %s", fileResp.Response, fileResp.Message)
	}

	return fileResp.FileID, nil
}

// uploadPart encrypts and uploads a single part.
func (p *UploadPackage) uploadPart(fileID string, part int, encryptedData []byte) error {
	// Get upload URL
	uploadURL, err := p.getUploadURL(fileID, part)
	if err != nil {
		return err
	}

	// Upload to S3 presigned URL with retries
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		req, err := http.NewRequest("PUT", uploadURL, bytes.NewReader(encryptedData))
		if err != nil {
			return err
		}

		resp, err := p.client.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("S3 upload failed: %d", resp.StatusCode)
			continue
		}

		return nil
	}

	return lastErr
}

// getUploadURL gets a presigned S3 URL for uploading a part.
func (p *UploadPackage) getUploadURL(fileID string, part int) (string, error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/%s/upload-urls/", p.PackageID, fileID)

	body := map[string]interface{}{
		"part": part,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("POST", urlPath, bodyBytes)
	if err != nil {
		return "", err
	}

	var urlResp struct {
		Response   string `json:"response"`
		Message    string `json:"message"`
		UploadUrls []struct {
			Part int    `json:"part"`
			URL  string `json:"url"`
		} `json:"uploadUrls"`
	}
	if err := json.Unmarshal(resp, &urlResp); err != nil {
		return "", err
	}

	if urlResp.Response != "SUCCESS" {
		return "", fmt.Errorf("get upload URL failed: %s - %s", urlResp.Response, urlResp.Message)
	}

	if len(urlResp.UploadUrls) == 0 {
		return "", fmt.Errorf("no upload URLs returned")
	}

	return urlResp.UploadUrls[0].URL, nil
}

// markUploadComplete marks a file upload as complete.
func (p *UploadPackage) markUploadComplete(fileID string) error {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/%s/upload-complete/", p.PackageID, fileID)

	resp, err := p.client.doRequest("POST", urlPath, nil)
	if err != nil {
		return err
	}

	var completeResp struct {
		Response string `json:"response"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(resp, &completeResp); err != nil {
		return err
	}

	if completeResp.Response != "SUCCESS" {
		return fmt.Errorf("mark complete failed: %s - %s", completeResp.Response, completeResp.Message)
	}

	return nil
}
