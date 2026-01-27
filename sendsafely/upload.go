package sendsafely

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dt/gosendsafely/util"
	"golang.org/x/sync/errgroup"
)

// Upload constants matching SendSafely protocol.
const (
	segmentSize = 2621440 // 2.5MB
)

// UploadToDropzone uploads files to a SendSafely dropzone in a single call.
// Returns the secure link on success.
// If label is non-empty, also submits to the hosted dropzone webhook (for Zendesk integration).
// The label is sent as the "name" field to the API (used for ticket IDs, submitter names, etc.).
func UploadToDropzone(
	dropzoneURL, dropzoneID, email, label string,
	progress func(name string, size util.BytesSize, mbps util.BytesSize, frac float64),
	paths ...string,
) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no files specified")
	}

	client := newdropzoneClient(dropzoneURL, dropzoneID)
	pkg, err := client.createPackage()
	if err != nil {
		return "", fmt.Errorf("create package: %w", err)
	}

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}

		name := filepath.Base(path)
		var lastUpdate time.Time
		var lastBytes int64

		_, err = pkg.uploadFile(path, func(bytes, total int64) {
			if progress != nil {
				now := time.Now()
				elapsed := now.Sub(lastUpdate)
				if elapsed > time.Second { 
					bytesSinceLast := bytes - lastBytes
					mbps := util.BytesSize(float64(bytesSinceLast) / elapsed.Seconds())
					frac := float64(bytes) / float64(total)
					progress(name, util.BytesSize(info.Size()), mbps, frac)
					lastUpdate = now
					lastBytes = bytes
				}
			}
		})
		if err != nil {
			return "", fmt.Errorf("upload %s: %w", path, err)
		}
	}

	link, err := pkg.finalize(email)
	if err != nil {
		return "", fmt.Errorf("finalize: %w", err)
	}

	// Submit to hosted dropzone if label provided
	if label != "" {
		if err := pkg.submitHostedDropzone(link, email, label); err != nil {
			// Don't fail the whole operation if dropzone submission fails
			// The link is still valid
			return link, fmt.Errorf("upload succeeded but dropzone submission failed: %w", err)
		}
	}

	return link, nil
}

// dropzoneClient handles anonymous dropzone uploads.
type dropzoneClient struct {
	baseURL    string
	dropzoneID string
	http       *http.Client
}

// newdropzoneClient creates a client for a specific dropzone.
func newdropzoneClient(baseURL, dropzoneID string) *dropzoneClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16

	return &dropzoneClient{
		baseURL:    baseURL,
		dropzoneID: dropzoneID,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// uploadPackage represents an in-progress upload.
type uploadPackage struct {
	client       *dropzoneClient
	packageID    string
	packageCode  string
	serverSecret string
	keyCode      string
}

// createPackage creates a new dropzone package for uploading.
func (c *dropzoneClient) createPackage() (*uploadPackage, error) {
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

	return &uploadPackage{
		client:       c,
		packageID:    pkgResp.PackageID,
		packageCode:  pkgResp.PackageCode,
		serverSecret: pkgResp.ServerSecret,
		keyCode:      keyCode,
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

// uploadChunk represents work for a single chunk upload.
type uploadChunk struct {
	part      int
	data      []byte
	uploadURL string // presigned S3 URL
}

// uploadFile uploads a file to the package with parallel chunk uploads.
func (p *uploadPackage) uploadFile(path string, progress func(bytes, total int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	name := filepath.Base(path)
	size := info.Size()
	parts := computeParts(size)

	// Create file entry
	fileID, err := p.createFile(name, parts, size)
	if err != nil {
		return "", err
	}

	// Fetch all upload URLs upfront
	uploadURLs := make(map[int]string, parts)
	const batchSize = 1000
	for i := 1; i <= parts; i += batchSize {
		endPart := min(i+batchSize-1, parts)
		urlBatch, err := p.getUploadURLsBatch(fileID, i, endPart)
		if err != nil {
			return "", fmt.Errorf("fetch upload URLs: %w", err)
		}
		for part, url := range urlBatch {
			uploadURLs[part] = url
		}
	}

	// Upload chunks in parallel
	passphrase := p.serverSecret + p.keyCode
	workCh := make(chan uploadChunk, 16)
	var uploadedBytes atomic.Int64

	g, ctx := errgroup.WithContext(context.Background())

	for range 16 {
		g.Go(func() error {
			for work := range workCh {
				// Encrypt chunk
				encrypted, err := encryptPGP(work.data, passphrase)
				if err != nil {
					return fmt.Errorf("encrypt part %d: %w", work.part, err)
				}

				// Upload directly to presigned URL (no API round trip)
				if err := p.uploadToS3(work.uploadURL, encrypted); err != nil {
					return fmt.Errorf("upload part %d: %w", work.part, err)
				}

				// Update progress counter (progress callback is called by ticker goroutine)
				uploadedBytes.Add(int64(len(work.data)))
			}
			return nil
		})
	}

	g.Go(func() error {
		defer close(workCh)
		for part := 1; part <= parts; part++ {
			offset, length := partBounds(part, size)

			// Read chunk data
			data := make([]byte, length)
			if _, err := f.ReadAt(data, offset); err != nil && err != io.EOF {
				return fmt.Errorf("read part %d: %w", part, err)
			}
			// Look up pre-fetched URL
			uploadURL, ok := uploadURLs[part]
			if !ok {
				return fmt.Errorf("missing upload URL for part %d", part)
			}

			select {
			case workCh <- uploadChunk{part: part, data: data, uploadURL: uploadURL}:
			case <-ctx.Done():
				return ctx.Err()
			}
			if progress != nil {
				progress(uploadedBytes.Load(), size)
			}
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return "", err
	}

	// Mark upload complete
	if err := p.markUploadComplete(fileID); err != nil {
		return "", fmt.Errorf("mark complete: %w", err)
	}

	return fileID, nil
}

// finalize completes the package and returns the secure link.
func (p *uploadPackage) finalize(email string) (secureLink string, err error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/finalize/", p.packageID)

	body := map[string]interface{}{
		"keyCode": p.keyCode,
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

// submitHostedDropzone submits to the dropzone webhook (for Zendesk integration etc.)
func (p *uploadPackage) submitHostedDropzone(secureLink, email, label string) error {
	urlPath := "/auth/json/"

	formData := url.Values{}
	formData.Set("action", "submitHostedDropzone")
	formData.Set("packageCode", p.packageCode)
	formData.Set("publicApiKey", p.client.dropzoneID)
	formData.Set("name", label)
	formData.Set("email", email)

	req, err := http.NewRequest("POST", p.client.baseURL+urlPath, bytes.NewBufferString(formData.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	resp, err := p.client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading dropzone response: %w", err)
	}

	var dropzoneResp struct {
		Success         string   `json:"success"`
		Error           string   `json:"error"`
		Data            string   `json:"data"`
		Digest          string   `json:"digest"`
		IntegrationUrls []string `json:"integrationUrls"`
	}
	if err := json.Unmarshal(body, &dropzoneResp); err != nil {
		return fmt.Errorf("parsing dropzone response: %w", err)
	}

	if dropzoneResp.Success != "true" {
		return fmt.Errorf("dropzone submission failed: %s", dropzoneResp.Error)
	}

	// Submit to integration webhooks (e.g., Zendesk)
	for _, integrationURL := range dropzoneResp.IntegrationUrls {
		webhookData := url.Values{}
		webhookData.Set("digest", dropzoneResp.Digest)
		webhookData.Set("data", dropzoneResp.Data)
		webhookData.Set("secureLink", secureLink)

		webhookReq, err := http.NewRequest("POST", integrationURL, bytes.NewBufferString(webhookData.Encode()))
		if err != nil {
			continue // skip failed webhooks like JS does
		}
		webhookReq.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

		webhookResp, err := p.client.http.Do(webhookReq)
		if err != nil {
			continue
		}
		webhookResp.Body.Close()
	}

	return nil
}

// doRequest performs an HTTP request to the dropzone API.
func (c *dropzoneClient) doRequest(method, urlPath string, body []byte) ([]byte, error) {
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

// computeParts calculates part count.
func computeParts(size int64) int {
	if size == 0 {
		return 1
	}
	return int((size + int64(segmentSize) - 1) / int64(segmentSize))
}

// partBounds returns (offset, length) for a given part number (1-indexed).
func partBounds(part int, fileSize int64) (offset, length int64) {
	offset = int64(part-1) * int64(segmentSize)
	length = min(int64(segmentSize), fileSize-offset)
	return offset, length
}

// createFile creates a file entry in the package.
func (p *uploadPackage) createFile(name string, parts int, size int64) (string, error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/", p.packageID)

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

// uploadToS3 uploads encrypted data directly to a presigned S3 URL with retries.
func (p *uploadPackage) uploadToS3(uploadURL string, encryptedData []byte) error {
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

// getUploadURLsBatch fetches presigned upload URLs for a range of parts.
// Returns a map of part number to presigned URL.
func (p *uploadPackage) getUploadURLsBatch(fileID string, startPart, endPart int) (map[int]string, error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/%s/upload-urls/", p.packageID, fileID)

	// Try batch request first (similar to download URL batching)
	body := map[string]interface{}{
		"startPart": startPart,
		"endPart":   endPart,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("POST", urlPath, bodyBytes)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	if urlResp.Response != "SUCCESS" {
		return nil, fmt.Errorf("get upload URLs failed: %s - %s", urlResp.Response, urlResp.Message)
	}

	// Build map of part -> URL
	urlMap := make(map[int]string, len(urlResp.UploadUrls))
	for _, entry := range urlResp.UploadUrls {
		urlMap[entry.Part] = entry.URL
	}

	return urlMap, nil
}

// markUploadComplete marks a file upload as complete.
func (p *uploadPackage) markUploadComplete(fileID string) error {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/%s/upload-complete/", p.packageID, fileID)

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
