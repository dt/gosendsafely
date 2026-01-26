package ss

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/zalando/go-keyring"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

type ID string

type File = ChunkedFile[ID]

type FileInfo struct {
	Name string
	Size BytesSize
}

type client struct {
	baseURL   string
	apiKey    string
	apiSecret string
	http      *http.Client
}

type Credentials struct {
	APIKey    string
	APISecret string
}

const keyringService = "sendsafely"

// CredentialOptions configures credential loading behavior.
type CredentialOptions struct {
	NoKeyring bool   // Skip keyring lookup and don't offer to save
	BaseURL   string // Base URL for API validation (required for validation)
}

type credentialSource int

const (
	sourceEnv credentialSource = iota
	sourceKeyring
	sourcePrompt
)

// LoadCredentials loads SendSafely API credentials from (in order):
// 1. Environment variables (SS_API_KEY, SS_API_SECRET)
// 2. System keyring (unless NoKeyring is set)
// 3. Interactive prompt (if terminal), with option to save to keyring
func LoadCredentials(opts CredentialOptions) (*Credentials, error) {
	creds, _, err := loadCredentialsWithSource(opts)
	return creds, err
}

func loadCredentialsWithSource(opts CredentialOptions) (*Credentials, credentialSource, error) {
	// 1. Environment variables
	if key, secret := os.Getenv("SS_API_KEY"), os.Getenv("SS_API_SECRET"); key != "" && secret != "" {
		return &Credentials{APIKey: key, APISecret: secret}, sourceEnv, nil
	}

	// 2. System keyring
	if !opts.NoKeyring {
		if creds, err := loadCredentialsFromKeyring(); err == nil {
			return creds, sourceKeyring, nil
		}
	}

	// 3. Interactive prompt
	creds, err := promptAndSaveCredentials(opts, "No SendSafely credentials found in SS_API_KEY/SS_API_SECRET or system keychain.")
	if err != nil {
		return nil, sourcePrompt, err
	}
	return creds, sourcePrompt, nil
}

func promptAndSaveCredentials(opts CredentialOptions, message string) (*Credentials, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("credentials not found: set SS_API_KEY and SS_API_SECRET environment variables")
	}

	fmt.Fprintln(os.Stderr, message)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "To get API credentials:")
	fmt.Fprintln(os.Stderr, "  1. Log in to SendSafely and go to Edit Profile")
	fmt.Fprintln(os.Stderr, "  2. Under API Keys, click 'Generate New Key'")
	fmt.Fprintln(os.Stderr, "  3. Copy the API Key and API Secret")
	fmt.Fprintln(os.Stderr, "")

	for {
		creds, err := promptForCredentials()
		if err != nil {
			return nil, err
		}

		// Validate before offering to save
		if opts.BaseURL != "" {
			fmt.Fprint(os.Stderr, "Validating credentials... ")
			email, err := creds.Validate(opts.BaseURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed: %v\n", err)
				fmt.Fprintln(os.Stderr, "Please try again.")
				fmt.Fprintln(os.Stderr, "")
				continue
			}
			fmt.Fprintf(os.Stderr, "ok (%s)\n", email)
		}

		if !opts.NoKeyring && promptYesNo("Save credentials to secure keychain?") {
			if err := saveCredentialsToKeyring(creds); err != nil {
				fmt.Fprintf(os.Stderr, "warning: couldn't save to keyring: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "Credentials saved to keychain.")
			}
		}

		return creds, nil
	}
}

// ForgetCredentials removes saved credentials from the system keyring.
func ForgetCredentials() error {
	var errs []error
	if err := keyring.Delete(keyringService, "api_key"); err != nil && err != keyring.ErrNotFound {
		errs = append(errs, err)
	}
	if err := keyring.Delete(keyringService, "api_secret"); err != nil && err != keyring.ErrNotFound {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to delete credentials: %v", errs)
	}
	return nil
}

// Validate checks if the credentials are valid by making a test API call.
// Returns the user's email on success for confirmation.
func (c *Credentials) Validate(baseURL string) (string, error) {
	cl := &client{
		baseURL:   baseURL,
		apiKey:    c.APIKey,
		apiSecret: c.APISecret,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
	resp, err := cl.doRequest("GET", "/api/v2.0/user/", nil)
	if err != nil {
		return "", err
	}
	var userInfo struct {
		Email    string `json:"email"`
		Response string `json:"response"`
	}
	if err := json.Unmarshal(resp, &userInfo); err != nil {
		return "", err
	}
	if userInfo.Response != "SUCCESS" {
		return "", fmt.Errorf("API returned: %s", userInfo.Response)
	}
	return userInfo.Email, nil
}

func loadCredentialsFromKeyring() (*Credentials, error) {
	key, err := keyring.Get(keyringService, "api_key")
	if err != nil {
		return nil, err
	}
	secret, err := keyring.Get(keyringService, "api_secret")
	if err != nil {
		return nil, err
	}
	return &Credentials{APIKey: key, APISecret: secret}, nil
}

func saveCredentialsToKeyring(c *Credentials) error {
	if err := keyring.Set(keyringService, "api_key", c.APIKey); err != nil {
		return err
	}
	return keyring.Set(keyringService, "api_secret", c.APISecret)
}

func promptForCredentials() (*Credentials, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Fprint(os.Stderr, "SendSafely API Key: ")
	key, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read API key: %w", err)
	}

	fmt.Fprint(os.Stderr, "SendSafely API Secret: ")
	secretBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after hidden input
	if err != nil {
		return nil, fmt.Errorf("failed to read API secret: %w", err)
	}

	return &Credentials{
		APIKey:    strings.TrimSpace(key),
		APISecret: strings.TrimSpace(string(secretBytes)),
	}, nil
}

func promptYesNo(prompt string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	answer, _ := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// OpenPackage parses a SendSafely URL, loads credentials, and fetches the list
// of files in the package. The returned package can be used to list or open the
// individual files.
func OpenPackage(rawURL string, lim Sem, credOpts CredentialOptions) (*Package, error) {
	// Strip backslash escapes (iTerm2 and other terminals escape special chars when pasting)
	rawURL = strings.ReplaceAll(rawURL, `\`, "")

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	// Extract packageCode from query params
	packageCode := u.Query().Get("packageCode")
	if packageCode == "" {
		// Try path-based format: /receive/?packageCode=XXX
		return nil, fmt.Errorf("packageCode not found in URL")
	}

	// Extract keyCode from fragment
	fragment := u.Fragment
	keyCode := ""
	if strings.HasPrefix(fragment, "keycode=") || strings.HasPrefix(fragment, "keyCode=") {
		keyCode = fragment[8:]
	} else if strings.Contains(fragment, "keycode=") {
		re := regexp.MustCompile(`keycode=([^&]+)`)
		matches := re.FindStringSubmatch(fragment)
		if len(matches) > 1 {
			keyCode = matches[1]
		}
	} else if strings.Contains(fragment, "keyCode=") {
		re := regexp.MustCompile(`keyCode=([^&]+)`)
		matches := re.FindStringSubmatch(fragment)
		if len(matches) > 1 {
			keyCode = matches[1]
		}
	}

	if keyCode == "" {
		return nil, fmt.Errorf("keyCode not found in URL fragment")
	}

	baseURL := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

	// Set BaseURL for credential validation
	credOpts.BaseURL = baseURL
	creds, source, err := loadCredentialsWithSource(credOpts)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16

	cl := &client{
		baseURL:   baseURL,
		apiKey:    creds.APIKey,
		apiSecret: creds.APISecret,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
	pkg, err := cl.getPackage(packageCode, keyCode)
	if err != nil {
		// If auth failed and credentials came from keyring, prompt for new ones
		if strings.Contains(err.Error(), "AUTHENTICATION_FAILED") && source == sourceKeyring {
			fmt.Fprintln(os.Stderr, "")
			creds, err = promptAndSaveCredentials(credOpts, "Saved credentials are invalid or expired.")
			if err != nil {
				return nil, err
			}
			cl.apiKey = creds.APIKey
			cl.apiSecret = creds.APISecret
			pkg, err = cl.getPackage(packageCode, keyCode)
		}
		if err != nil {
			return nil, err
		}
	}
	pkg.lim = lim
	return pkg, nil
}

func (c *client) doRequest(method, urlPath string, body []byte) ([]byte, error) {
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05+0000")

	bodyStr := ""
	if body != nil {
		bodyStr = string(body)
	}

	data := c.apiKey + urlPath + timestamp + bodyStr
	h := hmac.New(sha256.New, []byte(c.apiSecret))
	h.Write([]byte(data))
	signature := hex.EncodeToString(h.Sum(nil))

	fullURL := c.baseURL + urlPath
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("ss-api-key", c.apiKey)
	req.Header.Set("ss-request-signature", signature)
	req.Header.Set("ss-request-timestamp", timestamp)
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
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

type Package struct {
	client *client
	info struct {
		PackageID      string     `json:"packageId"`
		PackageCode    string     `json:"packageCode"`
		ServerSecret   string     `json:"serverSecret"`
		Files          []struct {
			FileID   string `json:"fileId"`
			FileName string `json:"fileName"`
			FileSize string `json:"fileSize"`
			Parts    int `json:"parts"`
		} `json:"files"`
		Response       string     `json:"response"`
	}
	keyCode string
	lim 	 Sem
}

func (c *client) getPackage(packageCode, keyCode string) (*Package, error) {
	urlPath := fmt.Sprintf("/api/v2.0/package/%s/", packageCode)
	resp, err := c.doRequest("GET", urlPath, nil)
	if err != nil {
		return nil, err
	}

	res := Package{client: c, keyCode: keyCode, }

	if err := json.Unmarshal(resp, &res.info); err != nil {
		return nil, fmt.Errorf("failed to parse package info: %w", err)
	}

	if res.info.Response != "SUCCESS" {
		return nil, fmt.Errorf("API returned: %s", res.info.Response)
	}

	return &res, nil
}

// Files returns the list of files in the package.
func (p *Package) Files() []FileInfo {
	files := make([]FileInfo, len(p.info.Files))
	for i, f := range p.info.Files {
		size, _ := strconv.Atoi(f.FileSize)
		files[i] = FileInfo{Name: f.FileName, Size: BytesSize(size)}
	}
	return files
}

// Open opens a file from the package for reading. The returned File supports
// chunked streaming downloads with automatic decryption.
func (p *Package) Open(fileName string) (*File, error) {
	var fileID string
	var parts int
	var fileSize int

	for _, f := range p.info.Files {
		if f.FileName == fileName {
			fileID = f.FileID
			parts = f.Parts
			var err error
			fileSize, err = strconv.Atoi(f.FileSize)
			if err != nil {
				return nil, fmt.Errorf("invalid fileSize value for file %s: %w", fileName, err)
			}
			break
		}
	}

	if fileID == "" {
		return nil, fmt.Errorf("file %s not found in package", fileName)
	}

	chunks := make([]Chunk[ID], parts)

	dk, err := pbkdf2.Key(sha256.New, p.keyCode, []byte(p.info.PackageCode), 1024, 32)
	if err != nil {
		return nil,  err
	}
	checksum := hex.EncodeToString(dk)

	const batchSize = 1000
	for i := 0; i < parts; i += batchSize {
		urlPath := fmt.Sprintf("/api/v2.0/package/%s/file/%s/download-urls/", p.info.PackageID, fileID)

		body := map[string]interface{}{
			"checksum":     checksum,
			"startSegment": i + 1,
			"endSegment":   min(i+batchSize, parts),
		}
		bodyBytes, _ := json.Marshal(body)

		resp, err := p.client.doRequest("POST", urlPath, bodyBytes)
		if err != nil {
			return nil, err
		}

		var dlResp struct {
			Response     string        `json:"response"`
			DownloadUrls []struct {
				Part int    `json:"part"`
				URL  string `json:"url"`
			} `json:"downloadUrls"`
		}

		if err := json.Unmarshal(resp, &dlResp); err != nil {
			return nil, fmt.Errorf("failed to parse download URLs: %w", err)
		}

		if dlResp.Response != "SUCCESS" {
			return nil, fmt.Errorf("API returned: %s", dlResp.Response)
		}

		for j := range dlResp.DownloadUrls {
			chunks[i+j].Idx = i + j
			chunks[i+j].ID = ID(dlResp.DownloadUrls[j].URL)
			chunks[i+j].lim = p.lim
		}
	}

	// Fetch the first and second chunks to determine chunk sizes and offsets; we
	// need to fetch the second since the first can have a unique size.
	c0, err := p.fetchAndDecrypt(chunks[0].ID)
	if err != nil {
		return nil, fmt.Errorf("failed to download chunk 0: %w", err)
	}
	// ref the chunk to prepare it to have content set; it'll never unref in effect
	// we're just pinning the chunk.
	chunks[0].ref()
	chunks[0].setContent(c0)

	chunks[0].offset = 0
	chunks[0].length = len(c0)

	if len(chunks) > 1 {
		c1, err := p.fetchAndDecrypt(chunks[1].ID)
		if err != nil {
			return nil, fmt.Errorf("failed to download chunk 1: %w", err)
		}
		chunks[1].ref()
		chunks[1].setContent(c1)

		// Set all chunks after the first to the size of the second chunk -- we'll
		// adjust the last chunk size later.
		for i := range chunks {
			if i > 0 {
				chunks[i].length = len(c1)
				chunks[i].offset = chunks[i-1].offset + chunks[i-1].length
			}
		}

		if len(chunks) > 2 {
			cLast, err := p.fetchAndDecrypt(chunks[len(chunks)-1].ID)
			if err != nil {
				return nil, fmt.Errorf("failed to download last chunk: %w", err)
			}
			chunks[len(chunks)-1].ref()
			chunks[len(chunks)-1].setContent(cLast)
			chunks[len(chunks)-1].length = len(cLast)
			if delta := len(cLast) - (fileSize - chunks[len(chunks)-1].offset); delta != 0 {
				fmt.Printf("\nWARNING: last chunk content differs from expected size %s; file may be truncated.\n", BytesSize(delta))
				fileSize += delta
			}
		}
	}


	file := &File{
		name:   fileName,
		chunks: chunks,
		size:   fileSize,
	}
	file.fetcher = func(id ID) ([]byte, error) {
		return p.fetchAndDecrypt(id)
	}
	return file, nil
}

func (c *client) fetchAttempt(url ID) ([]byte, error) {
	resp, err := c.http.Get(string(url))
		if err != nil {
			return nil, fmt.Errorf("failed to download chunk: %w", err)
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read chunk data: %w", err)
		}

		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("failed to download chunk, server reply %d: %s", resp.StatusCode, string(data[:min(200, len(data))]))
		}
		return data, nil
	}
	
func (p *Package) fetchAndDecrypt(url ID) ([]byte, error) {
	var data []byte
	var err error
	for i := 0; i < 5; i++ {
		data, err = p.client.fetchAttempt(url)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}

	md, err := openpgp.ReadMessage(bytes.NewReader(data), nil, func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		return []byte(p.info.ServerSecret + p.keyCode), nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read PGP message: %w", err)
	}

	plaintext, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, fmt.Errorf("failed to read decrypted data: %w", err)
	}

	return plaintext, nil
}

// DownloadFile downloads a file from the package to the specified output path.
// It supports resumption: if a partial download exists (.ssget-tmp file), it will
// resume from where it left off (with a 4MB safety margin for torn blocks).
func (p *Package) DownloadFile(fileName, outputPath string, progress func(string, int, float64),
) error {
	if progress != nil {
		progress("fetching metadata", 0, 0)
	}

	file, err := p.Open(fileName)
	if err != nil {
		return err
	}
	// Create output directory if needed
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	// Write to temp file, then rename
	tmpPath := outputPath + ".ssget-tmp"

	// Check for existing partial download and calculate resume offset
	const safetyMargin = 4 << 20 // 4MB safety margin for torn blocks
	resumeOffset := 0

	if info, err := os.Stat(tmpPath); err == nil {
		existingSize := int(info.Size())
		if existingSize > safetyMargin {
			resumeOffset = existingSize - safetyMargin
		}
		// If existingSize <= safetyMargin, resumeOffset stays 0 (start over)
	}

	var f *os.File
	if resumeOffset > 0 {
		// Truncate and open for append at the resume offset
		f, err = os.OpenFile(tmpPath, os.O_RDWR, 0644)
		if err != nil {
			return err
		}
		if err := f.Truncate(int64(resumeOffset)); err != nil {
			f.Close()
			return err
		}
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
			f.Close()
			return err
		}
	} else {
		// Create new file (or truncate existing to 0)
		f, err = os.Create(tmpPath)
		if err != nil {
			return err
		}
	}

	// Create reader starting at the resume offset
	reader := file.reader(span{offset: resumeOffset, length: file.size - resumeOffset})

	g, ctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		return file.FetchChunks(ctx, p.lim)
	})

	g.Go(func() error {
		if progress != nil {
			// Show initial progress accounting for resumed bytes
			frac := float64(resumeOffset) / float64(file.size)
			progress("downloading", 0, frac)
		}
		defer reader.Close()
		n := 0
		remaining := file.size - resumeOffset
		const chunkSize = 64 << 20
		lastProg := time.Now()
		var lastN int

		for n < remaining {
			m, err := io.CopyN(f, reader, int64(min(remaining-n, chunkSize)))
			if err != nil && err != io.EOF {
				return err
			}
			n += int(m)
			if progress != nil && time.Since(lastProg) > time.Second {
				elapsed := time.Since(lastProg).Seconds()
				bps := float64(n-lastN) / elapsed
				frac := float64(resumeOffset+n) / float64(file.size)
				progress("downloading", int(bps), frac)
				lastProg = time.Now()
				lastN = n
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, outputPath)
}

