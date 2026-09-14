// Package client provides HTTP helpers for the bulk-uploader and bulk-deleter tools
// to communicate with the labriideas server.
package client

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/joho/godotenv" // NEW: so local tools can read a .env file like the server does
)

// Client wraps the base URL and credentials of the labriideas server.
type Client struct {
	BaseURL string

	// NEW: Bearer token sent on every request. Admin endpoints on the server
	// require it (or a browser session cookie); public endpoints ignore it.
	// Attaching it uniformly means callers never need to think about which
	// routes are protected — the server decides.
	apiToken string
}

// New creates a new client with an empty base URL and no token.
// Retained for compatibility; prefer NewFromEnv.
func New() *Client {
	return &Client{BaseURL: ""}
}

// NewFromEnv creates a client using:
//   - API_BASE_URL env var (falls back to the production server)
//   - PUBLISHER_API_TOKEN env var (the admin Bearer token)
//
// NEW: loads a local .env file first (if present), so ad-hoc CLI tools can
// keep their credentials in an ignored .env beside the binary, matching how
// the server itself is configured. System env vars still take precedence
// because godotenv does not overwrite existing variables.
func NewFromEnv() *Client {
	_ = godotenv.Load() // Missing .env is not an error — fall back to system env

	base := osGetenv("API_BASE_URL")
	if base == "" {
		base = "https://labriideas-api.onrender.com"
		// base = "http://localhost:8080" // For local testing
	}

	token := osGetenv("PUBLISHER_API_TOKEN")
	if token == "" {
		// Warn loudly: without a token, every admin endpoint returns 401 and
		// the tool fails deep into a run with a confusing error.
		fmt.Fprintln(os.Stderr, "⚠️  PUBLISHER_API_TOKEN is not set — admin endpoints (crawl, upload, delete, update) will return 401.")
	}

	return &Client{BaseURL: base, apiToken: token}
}

// osGetenv is split out for testability.
func osGetenv(key string) string {
	return os_getenv(key)
}

// os_getenv is an indirection so we can mock env in tests.
var os_getenv = func(key string) string {
	return getenv(key)
}

// =============================================================================
// NEW: TRANSPORT LAYER
// =============================================================================
// Every request the client makes funnels through do(), which attaches the
// Bearer token. This is the single injection point for CLI authentication —
// no individual tool or method needs credential logic.

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	return http.DefaultClient.Do(req)
}

// get builds a GET request and routes it through do().
func (c *Client) get(rawURL string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// post builds a JSON POST request and routes it through do().
func (c *Client) post(rawURL string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

// statusError converts a non-2xx response into a descriptive error.
// NEW: 401 gets its own message so a bad/missing token is immediately
// diagnosable instead of surfacing as "server returned 401" mid-bulk-run.
func statusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (401) — check PUBLISHER_API_TOKEN")
	}
	return fmt.Errorf("server returned %d", resp.StatusCode)
}

// ==========================================
// CATALOG
// ==========================================

// CatalogTracks is a minimal struct of what we need from /api/catalog.
type CatalogTracks struct {
	Tracks []map[string]interface{} `json:"tracks"`
}

// FetchCatalog retrieves the current catalog from the server.
// Returns:
//   - catalogMap: filename -> metadata map
//   - catalogByHash: audio-hash -> filename
//   - error
func (c *Client) FetchCatalog() (map[string]map[string]string, map[string]string, error) {
	empty := make(map[string]map[string]string)
	emptyHash := make(map[string]string)

	// CHANGED: routed through c.get so the token attaches (harmless here —
	// catalog is public — but keeps every call uniform).
	resp, err := c.get(c.BaseURL + "/api/catalog")
	if err != nil {
		return empty, emptyHash, err
	}
	defer resp.Body.Close()

	// Handle fresh bucket or server error
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusInternalServerError {
		return empty, emptyHash, nil
	}
	if resp.StatusCode != http.StatusOK {
		return empty, emptyHash, statusError(resp)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return empty, emptyHash, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for gzip magic bytes
	if len(bodyBytes) >= 2 && bodyBytes[0] == 0x1f && bodyBytes[1] == 0x8b {
		gzReader, err := gzip.NewReader(bytes.NewReader(bodyBytes))
		if err != nil {
			return empty, emptyHash, fmt.Errorf("failed to init gzip reader: %w", err)
		}
		defer gzReader.Close()
		bodyBytes, err = io.ReadAll(gzReader)
		if err != nil {
			return empty, emptyHash, fmt.Errorf("failed to decompress gzip: %w", err)
		}
	}

	var data CatalogTracks
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		return empty, emptyHash, fmt.Errorf("failed to parse catalog JSON: %w", err)
	}

	catalogMap := make(map[string]map[string]string)
	catalogByHash := make(map[string]string)

	for _, track := range data.Tracks {
		filename, ok := track["filename"].(string)
		if !ok {
			continue
		}
		metaMap := make(map[string]string)
		for k, v := range track {
			if k == "id" || k == "filename" || k == "hash" {
				continue
			}
			metaMap[k] = fmt.Sprintf("%v", v)
		}
		catalogMap[filename] = metaMap
		if audioHash, exists := metaMap["audio-hash"]; exists && audioHash != "" {
			catalogByHash[audioHash] = filename
		}
	}
	return catalogMap, catalogByHash, nil
}

// ==========================================
// CRAWL
// ==========================================

// TriggerCrawl starts an asynchronous crawl on the server and returns the job ID.
func (c *Client) TriggerCrawl() (string, error) {
	// CHANGED: routed through c.post for token attachment.
	resp, err := c.post(c.BaseURL+"/api/start-crawl", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return "", statusError(resp)
	}

	var jobResp struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		return "", err
	}
	return jobResp.JobID, nil
}

// PollCrawlStatus blocks until the job is completed or failed.
func (c *Client) PollCrawlStatus(jobID string) error {
	for {
		// CHANGED: routed through c.get for token attachment.
		resp, err := c.get(fmt.Sprintf("%s/api/crawl-status?job_id=%s", c.BaseURL, jobID))
		if err != nil {
			timeSleep(1)
			continue
		}

		// FIXED: this endpoint is now admin-protected. Without this check a
		// rejected (401) poll would decode an error body into an empty status
		// and loop forever printing "0%". Auth failures abort immediately.
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return statusError(resp)
		}

		var status struct {
			Status   string `json:"status"`
			Progress int    `json:"progress"`
			Message  string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()

		fmt.Printf("\r  Server Progress: %d%% - %s", status.Progress, status.Message)

		switch status.Status {
		case "completed":
			fmt.Println("\n✅ Catalog rebuild finished on server.")
			return nil
		case "failed":
			return fmt.Errorf("crawl failed: %s", status.Message)
		}
		timeSleep(1)
	}
}

// ==========================================
// TRACK OPERATIONS
// ==========================================

// DeleteTrack calls POST /api/delete-track on the server.
func (c *Client) DeleteTrack(filename string) error {
	payload := map[string]string{"filename": filename}
	jsonPayload, _ := json.Marshal(payload)

	// CHANGED: routed through c.post for token attachment. This endpoint is
	// admin-protected on the server, so without the token this call 401s.
	resp, err := c.post(c.BaseURL+"/api/delete-track", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return nil
}

// GetSignedUploadURL calls GET /api/get-upload-url and returns the presigned URL.
func (c *Client) GetSignedUploadURL(filename string) (string, error) {
	encoded := url.QueryEscape(filename)

	// CHANGED: routed through c.get for token attachment. This endpoint is
	// admin-protected on the server, so without the token this call 401s.
	resp, err := c.get(c.BaseURL + "/api/get-upload-url?filename=" + encoded)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// FIXED: this endpoint is admin-protected. Previously a 401 response body
	// ({"error":"unauthorized"}) would decode into an empty URL and surface
	// as the misleading "empty presigned url received" error. Now the real
	// cause is reported.
	if resp.StatusCode != http.StatusOK {
		return "", statusError(resp)
	}

	var urlResp struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&urlResp); err != nil {
		return "", err
	}
	if urlResp.URL == "" {
		return "", fmt.Errorf("empty presigned url received")
	}
	return urlResp.URL, nil
}

// UpdateMetadata calls POST /api/update-metadata.
func (c *Client) UpdateMetadata(filename string, metadata map[string]string) error {
	payload := map[string]interface{}{
		"filename": filename,
		"metadata": metadata,
	}
	jsonPayload, _ := json.Marshal(payload)

	// CHANGED: routed through c.post for token attachment. This endpoint is
	// admin-protected on the server.
	resp, err := c.post(c.BaseURL+"/api/update-metadata", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return nil
}

// ==========================================
// HELPERS
// ==========================================

// MetadataMatches compares CSV metadata to R2 metadata (case-insensitive, trimmed).
func MetadataMatches(csvMeta, r2Meta map[string]string) bool {
	for k, csvVal := range csvMeta {
		r2Val, exists := r2Meta[k]
		if !exists {
			return false
		}
		if strings.TrimSpace(strings.ToLower(csvVal)) != strings.TrimSpace(strings.ToLower(r2Val)) {
			return false
		}
	}
	return true
}
