package sendsafely

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
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
	onComplete func(name string, size util.BytesSize, duration time.Duration),
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
		size := util.BytesSize(info.Size())
		fileStart := time.Now()
		var lastUpdate time.Time

		_, err = pkg.uploadFile(path, func(bytes, total int64) {
			if progress != nil {
				now := time.Now()
				if now.Sub(lastUpdate) > time.Second {
					elapsed := now.Sub(fileStart).Seconds()
					mbps := util.BytesSize(float64(bytes) / elapsed)
					frac := float64(bytes) / float64(total)
					progress(name, size, mbps, frac)
					lastUpdate = now
				}
			}
		})
		if err != nil {
			return "", fmt.Errorf("upload %s: %w", path, err)
		}

		if onComplete != nil {
			onComplete(name, size, time.Since(fileStart))
		}
	}

	// Distribute keycodes to package recipients before finalizing.
	_ = pkg.distributeKeycodes()

	link, err := pkg.finalize(email)
	if err != nil {
		return "", fmt.Errorf("finalize: %w", err)
	}

	if err := pkg.submitHostedDropzone(link, email, label); err != nil {
		// Don't fail the whole operation if dropzone submission fails
		// The link is still valid
		return link, fmt.Errorf("upload succeeded but dropzone submission failed: %w", err)
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

	createBody, _ := json.Marshal(map[string]interface{}{"vdr": false})
	resp, err := c.doRequest("PUT", urlPath, createBody)
	if err != nil {
		return nil, fmt.Errorf("create package: %w", err)
	}

	var pkgResp struct {
		Response     string `json:"response"`
		Message      string `json:"message"`
		ErrorID      string `json:"errorId"`
		PackageID    string `json:"packageId"`
		PackageCode  string `json:"packageCode"`
		ServerSecret string `json:"serverSecret"`
	}
	if err := json.Unmarshal(resp, &pkgResp); err != nil {
		return nil, fmt.Errorf("parse create package response: %w", err)
	}

	if pkgResp.Response != "SUCCESS" {
		if pkgResp.ErrorID != "" {
			return nil, fmt.Errorf("create package failed: %s - %s (errorId: %s)", pkgResp.Response, pkgResp.Message, pkgResp.ErrorID)
		}
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
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// uploadChunk represents work for a single chunk upload.
type uploadChunk struct {
	part int
	data []byte
	url  string // presigned S3 upload URL
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

	// Upload chunks in parallel
	passphrase := p.serverSecret + p.keyCode
	workCh := make(chan uploadChunk, 16)
	var uploadedBytes atomic.Int64

	g, ctx := errgroup.WithContext(context.Background())

	// Workers: encrypt and upload (URLs already fetched)
	for range 16 {
		g.Go(func() error {
			for work := range workCh {
				// Encrypt chunk
				encrypted, err := encryptPGP(work.data, passphrase)
				if err != nil {
					return fmt.Errorf("encrypt part %d: %w", work.part, err)
				}

				// Upload directly to presigned URL
				if err := p.uploadToS3(work.url, encrypted); err != nil {
					return fmt.Errorf("upload part %d: %w", work.part, err)
				}

				// Update progress
				uploaded := uploadedBytes.Add(int64(len(work.data)))
				if progress != nil {
					progress(uploaded, size)
				}
			}
			return nil
		})
	}

	// Reader: fetch URLs in batches, read data, send to workers
	g.Go(func() error {
		defer close(workCh)
		partURLs := make(map[int]string)

		for part := 1; part <= parts; part++ {
			// Fetch more URLs if needed
			if _, ok := partURLs[part]; !ok {
				urls, err := p.getUploadURLs(fileID, part)
				if err != nil {
					return fmt.Errorf("get upload URLs starting at part %d: %w", part, err)
				}
				for p, u := range urls {
					partURLs[p] = u
				}
			}

			url := partURLs[part]
			offset, length := partBounds(part, size)

			// Read chunk data
			data := make([]byte, length)
			if _, err := f.ReadAt(data, offset); err != nil && err != io.EOF {
				return fmt.Errorf("read part %d: %w", part, err)
			}

			select {
			case workCh <- uploadChunk{part: part, data: data, url: url}:
			case <-ctx.Done():
				return ctx.Err()
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

	// Compute checksum: PBKDF2(keyCode, packageCode, 1024 iterations, 32 bytes)
	dk, err := pbkdf2.Key(sha256.New, p.keyCode, []byte(p.packageCode), 1024, 32)
	if err != nil {
		return "", fmt.Errorf("compute checksum: %w", err)
	}
	checksum := hex.EncodeToString(dk)

	body := map[string]interface{}{
		"checksum":              checksum,
		"unconfirmedSender":     email,
		"undisclosedRecipients": false,
		"notifyRecipients":      false,
		"readOnlyPdf":           false,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("POST", urlPath, bodyBytes)
	if err != nil {
		return "", fmt.Errorf("finalize: %w", err)
	}

	var finResp struct {
		Response string `json:"response"`
		Message  string `json:"message"`
		ErrorID  string `json:"errorId"`
	}
	if err := json.Unmarshal(resp, &finResp); err != nil {
		return "", fmt.Errorf("parse finalize response: %w", err)
	}

	if finResp.Response != "SUCCESS" {
		if finResp.ErrorID != "" {
			return "", fmt.Errorf("finalize failed: %s - %s (errorId: %s)", finResp.Response, finResp.Message, finResp.ErrorID)
		}
		return "", fmt.Errorf("finalize failed: %s - %s", finResp.Response, finResp.Message)
	}

	// Construct the full secure link: message contains base URL, append keyCode
	secureLink = finResp.Message + "#keyCode=" + p.keyCode
	return secureLink, nil
}

// recipientKey represents a recipient's public key from the package.
type recipientKey struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// distributeKeycodes fetches recipient public keys and uploads the keycode
// encrypted for each recipient. This is required for recipients to access the
// package — without it, only the uploader can decrypt.
func (p *uploadPackage) distributeKeycodes() error {
	publicKeys, err := p.getPublicKeys()
	if err != nil {
		return fmt.Errorf("get public keys: %w", err)
	}

	for _, pk := range publicKeys {
		encryptedKeycode, err := p.encryptKeycodeForRecipient(pk.Key)
		if err != nil {
			return fmt.Errorf("encrypt keycode for recipient %s: %w", pk.ID, err)
		}

		if err := p.uploadKeycode(pk.ID, encryptedKeycode); err != nil {
			return fmt.Errorf("upload keycode for recipient %s: %w", pk.ID, err)
		}
	}

	return nil
}

// getPublicKeys fetches the public keys for all package recipients.
func (p *uploadPackage) getPublicKeys() ([]recipientKey, error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/public-keys/", p.packageCode)
	resp, err := p.client.doRequest("GET", urlPath, nil)
	if err != nil {
		return nil, err
	}

	var keysResp struct {
		Response   string         `json:"response"`
		Message    string         `json:"message"`
		PublicKeys []recipientKey `json:"publicKeys"`
	}
	if err := json.Unmarshal(resp, &keysResp); err != nil {
		return nil, err
	}

	if keysResp.Response != "SUCCESS" {
		return nil, fmt.Errorf("get public keys: %s - %s", keysResp.Response, keysResp.Message)
	}

	return keysResp.PublicKeys, nil
}

// encryptKeycodeForRecipient encrypts the keycode with a recipient's public key,
// returning the armored PGP message.
func (p *uploadPackage) encryptKeycodeForRecipient(armoredPublicKey string) (string, error) {
	entityList, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPublicKey))
	if err != nil {
		return "", fmt.Errorf("parse public key: %w", err)
	}

	var buf bytes.Buffer
	armorWriter, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return "", err
	}

	plaintext, err := openpgp.Encrypt(armorWriter, entityList, nil, nil, nil)
	if err != nil {
		return "", err
	}
	if _, err := plaintext.Write([]byte(p.keyCode)); err != nil {
		return "", err
	}
	if err := plaintext.Close(); err != nil {
		return "", err
	}
	if err := armorWriter.Close(); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// uploadKeycode uploads an encrypted keycode for a specific recipient.
func (p *uploadPackage) uploadKeycode(publicKeyID, encryptedKeycode string) error {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/link/%s/", p.packageCode, publicKeyID)

	body := map[string]interface{}{
		"keycode":          encryptedKeycode,
		"notifyRecipients": false,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("PUT", urlPath, bodyBytes)
	if err != nil {
		return err
	}

	var linkResp struct {
		Response string `json:"response"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(resp, &linkResp); err != nil {
		return err
	}

	if linkResp.Response != "SUCCESS" {
		return fmt.Errorf("upload keycode: %s - %s", linkResp.Response, linkResp.Message)
	}

	return nil
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

	// Submit to integration webhooks (e.g., Zendesk) if label provided.
	if label == "" {
		return nil
	}
	var webhookErrors []string
	for _, integrationURL := range dropzoneResp.IntegrationUrls {
		webhookData := url.Values{}
		webhookData.Set("digest", dropzoneResp.Digest)
		webhookData.Set("data", dropzoneResp.Data)
		webhookData.Set("secureLink", secureLink)

		const maxAttempts = 3
		var lastErr string
		for attempt := 0; attempt < maxAttempts; attempt++ {
			webhookReq, err := http.NewRequest("POST", integrationURL, bytes.NewBufferString(webhookData.Encode()))
			if err != nil {
				lastErr = err.Error()
				break
			}
			webhookReq.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

			webhookResp, err := p.client.http.Do(webhookReq)
			if err != nil {
				lastErr = err.Error()
				continue
			}
			body, _ := io.ReadAll(webhookResp.Body)
			webhookResp.Body.Close()

			var result struct {
				Result string `json:"result"`
			}
			if err := json.Unmarshal(body, &result); err == nil {
				switch strings.ToUpper(result.Result) {
				case "SUCCESS":
					goto nextWebhook
				case "ERROR":
					lastErr = fmt.Sprintf("webhook returned ERROR: %s", string(body))
					goto nextWebhook
				}
			}
			lastErr = fmt.Sprintf("unexpected webhook response: %s", string(body))
		}
		if lastErr != "" {
			webhookErrors = append(webhookErrors, lastErr)
		}
	nextWebhook:
	}
	if len(webhookErrors) > 0 {
		return fmt.Errorf("integration webhook errors: %s", strings.Join(webhookErrors, "; "))
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
	req.Header.Set("ss-request-api", "NODE_API")
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
		"filename":   name,
		"uploadType": "NODE_API",
		"parts":      parts,
		"filesize":   size,
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := p.client.doRequest("PUT", urlPath, bodyBytes)
	if err != nil {
		return "", err
	}

	var fileResp struct {
		Response string `json:"response"`
		Message  string `json:"message"`
		ErrorID  string `json:"errorId"`
		FileID   string `json:"fileId"`
	}
	if err := json.Unmarshal(resp, &fileResp); err != nil {
		return "", err
	}

	if fileResp.Response != "SUCCESS" {
		if fileResp.ErrorID != "" {
			return "", fmt.Errorf("create file failed: %s - %s (errorId: %s)", fileResp.Response, fileResp.Message, fileResp.ErrorID)
		}
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
		req.ContentLength = int64(len(encryptedData))

		resp, err := p.client.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("S3 upload failed: status %d", resp.StatusCode)
			continue
		}

		return nil
	}

	return lastErr
}

// getUploadURLs fetches presigned upload URLs starting from a given part.
// Returns a map of part number -> URL (up to 25 URLs).
func (p *uploadPackage) getUploadURLs(fileID string, startPart int) (map[int]string, error) {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/%s/upload-urls/", p.packageID, fileID)

	body := map[string]interface{}{
		"part":       startPart,
		"forceProxy": false,
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
		return nil, fmt.Errorf("get upload URLs failed: %s (raw: %s)", urlResp.Response, string(resp))
	}

	if len(urlResp.UploadUrls) == 0 {
		return nil, fmt.Errorf("no upload URLs returned starting at part %d", startPart)
	}

	urls := make(map[int]string, len(urlResp.UploadUrls))
	for _, u := range urlResp.UploadUrls {
		urls[u.Part] = u.URL
	}
	return urls, nil
}

// markUploadComplete marks a file upload as complete.
// Polls until the server confirms the file is fully processed.
func (p *uploadPackage) markUploadComplete(fileID string) error {
	urlPath := fmt.Sprintf("/drop-zone/v2.0/package/%s/file/%s/upload-complete/", p.packageID, fileID)

	body := map[string]interface{}{
		"complete": true,
	}
	bodyBytes, _ := json.Marshal(body)

	// Poll until server confirms complete (message == "true")
	for attempt := 0; attempt < 30; attempt++ {
		resp, err := p.client.doRequest("POST", urlPath, bodyBytes)
		if err != nil {
			return err
		}

		var completeResp struct {
			Response string `json:"response"`
			Message  string `json:"message"`
			ErrorID  string `json:"errorId"`
		}
		if err := json.Unmarshal(resp, &completeResp); err != nil {
			return err
		}

		if completeResp.Response != "SUCCESS" {
			if completeResp.ErrorID != "" {
				return fmt.Errorf("mark complete failed: %s - %s (errorId: %s)", completeResp.Response, completeResp.Message, completeResp.ErrorID)
			}
			return fmt.Errorf("mark complete failed: %s - %s", completeResp.Response, completeResp.Message)
		}

		// Server returns message="true" when file is fully processed
		if completeResp.Message == "true" {
			return nil
		}

		// Not ready yet, wait and retry (like JS SDK does)
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("mark complete timed out waiting for server confirmation")
}
