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
	"strings"
)

// Client wraps the base URL of the labriideas server.
type Client struct {
	BaseURL string
}

// New creates a new client. BaseURL is read from API_BASE_URL env var,
// falling back to the production server.
func New() *Client {
	base := ""
	// Use os.Getenv at the call site or via NewFromEnv; keeping this simple:
	// The caller can set Client.BaseURL directly if they want.
	return &Client{BaseURL: base}
}

// NewFromEnv creates a client using the API_BASE_URL env var,
// falling back to the production server if unset.
func NewFromEnv() *Client {
	base := osGetenv("API_BASE_URL")
	if base == "" {
		// base = "https://labriideas-api.onrender.com"
		base = "http://localhost:8080" // For local testing
	}
	return &Client{BaseURL: base}
}

// osGetenv is split out for testability.
func osGetenv(key string) string {
	return os_getenv(key)
}

// os_getenv is an indirection so we can mock env in tests.
var os_getenv = func(key string) string {
	return getenv(key)
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

	resp, err := http.Get(c.BaseURL + "/api/catalog")
	if err != nil {
		return empty, emptyHash, err
	}
	defer resp.Body.Close()

	// Handle fresh bucket or server error
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusInternalServerError {
		return empty, emptyHash, nil
	}
	if resp.StatusCode != http.StatusOK {
		return empty, emptyHash, fmt.Errorf("server returned %d", resp.StatusCode)
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
	resp, err := http.Post(c.BaseURL+"/api/start-crawl", "application/json", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("server returned %d", resp.StatusCode)
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
		resp, err := http.Get(fmt.Sprintf("%s/api/crawl-status?job_id=%s", c.BaseURL, jobID))
		if err != nil {
			timeSleep(1)
			continue
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

	resp, err := http.Post(c.BaseURL+"/api/delete-track", "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// GetSignedUploadURL calls GET /api/get-upload-url and returns the presigned URL.
func (c *Client) GetSignedUploadURL(filename string) (string, error) {
	encoded := url.QueryEscape(filename)
	resp, err := http.Get(c.BaseURL + "/api/get-upload-url?filename=" + encoded)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

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

	resp, err := http.Post(c.BaseURL+"/api/update-metadata", "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
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
