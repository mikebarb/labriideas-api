package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"

	"github.com/mikebarb/labriideas-publisher/pkg/cache"
	"github.com/mikebarb/labriideas-publisher/pkg/schema"
	"github.com/mikebarb/labriideas-publisher/pkg/storage"
)

// Global storage and cache singletons
var storageClient *storage.Client
var catalogCache *cache.CatalogCache

// In-memory store for background crawl tasks
var jobRegistry sync.Map

// Pre-cached schema bytes served to the frontend editor
var menuSchemaBytes []byte

// Request Payloads
type MetadataUpdateRequest struct {
	Filename string            `json:"filename"`
	Metadata map[string]string `json:"metadata"`
}

// UploadTrackRequest defines the metadata payload sent alongside the file
type UploadTrackRequest struct {
	Filename string            `json:"filename"`
	Metadata map[string]string `json:"metadata"`
}

// CrawlJob represents the state of an asynchronous crawl task
type CrawlJob struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // "running", "completed", "failed"
	Progress int    `json:"progress"`
	Message  string `json:"message"`
}

func init() {
	var err error
	// Read and verify menu JSON schema at application startup
	menuSchemaBytes, err = os.ReadFile("pkg/schema/menu.schema.json")
	if err != nil {
		log.Printf("⚠️  WARNING: Could not load pkg/schema/menu.schema.json: %v", err)
	}
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, relying on system env vars")
	}
	// --- NEW: Initialize Auth system ---
	InitAuth()
	if len(adminPasswords()) == 0 {
		log.Println("⚠️  WARNING: ADMIN_PASSWORDS is empty — browser login will fail.")
	}
	if len(adminAPITokens()) == 0 {
		log.Println("⚠️  WARNING: ADMIN_API_TOKENS is empty — CLI tools will be rejected.")
	}

	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	accountID := os.Getenv("R2_ACCOUNT_ID")
	bucketName := os.Getenv("R2_BUCKET_NAME")

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 1. Get the Port from the environment (Render provides this)
	// If it's not set (like on your local machine), default to 8080
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Create the raw S3 Client
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + accountID + ".r2.cloudflarestorage.com")
	})

	// Initialize YOUR Library Client!
	storageClient = storage.NewClient(s3Client, bucketName)
	catalogCache = cache.NewCatalogCache() // Initialize empty cache

	// Setup HTTP Routes
	mux := http.NewServeMux()

	// --- Public endpoints (no token required) ---
	mux.HandleFunc("/api/catalog", corsMiddleware(catalogHandler))
	mux.HandleFunc("/api/download", corsMiddleware(downloadHandler))
	mux.HandleFunc("/api/schema/menu", corsMiddleware(menuSchemaHandler))

	// --- Auth endpoints (must be reachable without a token) ---
	mux.HandleFunc("/api/auth/login", corsMiddleware(loginHandler))
	mux.HandleFunc("/api/auth/logout", corsMiddleware(logoutHandler))
	mux.HandleFunc("/api/auth/status", corsMiddleware(authStatusHandler))

	// --- Admin endpoints (Protected: Bearer token or Cookie session required) ---
	// Middleware order: CORS runs first (handles headers/preflight), then Auth (gates access)
	mux.HandleFunc("/api/upload", corsMiddleware(authMiddleware(uploadHandler)))
	mux.HandleFunc("/api/upload-track", corsMiddleware(authMiddleware(uploadTrackHandler)))
	mux.HandleFunc("/api/update-metadata", corsMiddleware(authMiddleware(updateMetadataHandler)))
	mux.HandleFunc("/api/delete-track", corsMiddleware(authMiddleware(deleteTrackHandler)))
	mux.HandleFunc("/api/get-upload-url", corsMiddleware(authMiddleware(getSignedUploadURLHandler)))
	mux.HandleFunc("/api/start-crawl", corsMiddleware(authMiddleware(startCrawlHandler)))
	mux.HandleFunc("/api/crawl-status", corsMiddleware(authMiddleware(crawlStatusHandler)))
	mux.HandleFunc("/api/update-menu", corsMiddleware(authMiddleware(updateMenuHandler)))

	// BACKGROUND CACHE WARMUP
	go func() {
		log.Println("Warming up catalog cache...")
		ctx := context.Background()

		// 1. Get ETag from R2
		head, err := storageClient.GetMetadata(ctx, "catalog.json.gz")
		if err != nil {
			log.Printf("Cache warmup failed (metadata): %v", err)
			return
		}
		r2ETag := *head.ETag

		// 2. Get compressed bytes from R2
		bytes, err := storageClient.GetObjectBytes(ctx, "catalog.json.gz")
		if err != nil {
			log.Printf("Cache warmup failed (download): %v", err)
			return
		}

		// 3. Update RAM Cache
		catalogCache.Update(r2ETag, bytes)
		log.Println("✅ Catalog cache warmed up successfully!")
	}()

	// --- Statistics collector: periodic flush to R2, final flush on shutdown ---
	// NOTE: signal.NotifyContext CAPTURES SIGINT/SIGTERM — once registered,
	// Ctrl+C no longer terminates the process by default. The signal only
	// cancels statsCtx. Therefore main() must observe the shutdown and stop
	// the HTTP server itself (below), or the process hangs forever with the
	// port held open. This is the whole point of the select at the end.
	statsCtx, statsStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer statsStop()

	// statsDone closes AFTER StartStatsLoop's final flush completes — main
	// waits on it so the server isn't killed mid-flush.
	//statsDone := make(chan struct{})
	//go func() {
	//	StartStatsLoop(statsCtx) // returns after final flush on shutdown
	//	close(statsDone)
	//}()

	// statsDone receives the final flush outcome (true = flushed, false =
	// requeued/failed) when StartStatsLoop returns on shutdown — main waits
	// on it so the server isn't torn down mid-flush, and uses the result
	// for an honest shutdown message.
	statsDone := make(chan bool, 1)
	go func() {
		ok := StartStatsLoop(statsCtx) // true if final flush succeeded
		statsDone <- ok
	}()

	// CHANGED: http.Server instead of http.ListenAndServe — Shutdown()
	// requires a server object; the package-level function can't be stopped.
	srv := &http.Server{Addr: ":" + port, Handler: mux}

	// Run the server in a goroutine so main can supervise both the server
	// and the shutdown signal.
	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.ListenAndServe() }()

	log.Printf("🚀 Server running on http://localhost:%s", port)

	select {
	case err := <-serverErr:
		// The server failed on its own (port bind error, etc.) — fatal.
		// ErrServerClosed is the normal return from Shutdown() and is NOT
		// an error.
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server listener failed: %v", err)
		}
	//case <-statsDone:
	// Signal received AND final stats flush complete. Now stop the
	// HTTP server: Shutdown closes the listener (main's select would
	// otherwise block on it) and waits up to 5s for in-flight requests
	// to finish before returning. Then main returns → process exits.
	//	log.Println("🛑 Shutting down (stats flushed)...")
	//	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	//	defer cancel()
	//	if err := srv.Shutdown(shutdownCtx); err != nil {
	//		log.Printf("Forcing listener close: %v", err)
	//	}
	case flushed := <-statsDone:
		// Signal received AND final stats flush ATTEMPT completed. Now stop
		// the HTTP server: Shutdown closes the listener (main's select
		// would otherwise block on it) and waits up to 5s for in-flight
		// requests to finish before returning. Then main returns → exit.
		if flushed {
			log.Println("🛑 Shutting down (stats flushed cleanly)...")
		} else {
			log.Println("🛑 Shutting down (stats flush failed — events requeued, will be lost at exit)...")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Forcing listener close: %v", err)
		}
	}
}

// =============================================================================
// HTTP HANDLERS
// =============================================================================

// --- MENU SCHEMA HANDLER ---
func menuSchemaHandler(w http.ResponseWriter, r *http.Request) {
	if len(menuSchemaBytes) == 0 {
		http.Error(w, "Menu schema not available on server", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(menuSchemaBytes)
}

// updateMenuHandler acts as the GitOps CI/CD orchestrator. It performs structural
// sanity checks on incoming menu data and commits it directly to the GitHub repository.
func updateMenuHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var menuData map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&menuData); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// ─── STRUCTURAL VALIDATION GATE ───
	// Verify that critical root keys exist before sending to GitHub.
	requiredRootKeys := []string{"subMenus", "featuredLectures", "schaefferCollection"}
	for _, key := range requiredRootKeys {
		if _, exists := menuData[key]; !exists {
			http.Error(w, fmt.Sprintf("Schema violation: missing root key '%s'", key), http.StatusBadRequest)
			return
		}
	}

	// Verify required sub-menus exist with exact naming.
	subMenus, ok := menuData["subMenus"].([]interface{})
	if !ok || len(subMenus) == 0 {
		http.Error(w, "Schema violation: 'subMenus' must be a non-empty array", http.StatusBadRequest)
		return
	}

	requiredSubMenus := []string{"Contact L'Abri", "Playlists", "Topics"}
	foundSubMenus := make(map[string]bool)

	for _, sm := range subMenus {
		if smMap, ok := sm.(map[string]interface{}); ok {
			if name, ok := smMap["subMenu"].(string); ok {
				foundSubMenus[name] = true
			}
		}
	}

	for _, req := range requiredSubMenus {
		if !foundSubMenus[req] {
			http.Error(w, fmt.Sprintf("Critical section missing: 'subMenu': '%s' was not found. Check naming.", req), http.StatusBadRequest)
			return
		}
	}

	// ─── GITHUB API COMMIT PIPELINE ───
	owner := os.Getenv("GITHUB_OWNER")
	repo := os.Getenv("GITHUB_REPO")
	branch := os.Getenv("GITHUB_BRANCH")
	if branch == "" {
		branch = "main"
	}
	token := os.Getenv("GITHUB_PAT")
	path := "site/src/data/menu.json"

	if owner == "" || repo == "" || token == "" {
		log.Println("[ERROR] GitHub configuration missing (GITHUB_OWNER, GITHUB_REPO, or GITHUB_PAT)")
		http.Error(w, "Server GitHub integration not configured", http.StatusInternalServerError)
		return
	}

	// 1. Fetch current file SHA from GitHub
	getURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, branch)
	getReq, err := http.NewRequestWithContext(r.Context(), "GET", getURL, nil)
	if err != nil {
		http.Error(w, "Failed to create GitHub request", http.StatusInternalServerError)
		return
	}
	getReq.Header.Set("Authorization", "Bearer "+token)
	getReq.Header.Set("Accept", "application/vnd.github.v3+json")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	getResp, err := httpClient.Do(getReq)
	if err != nil || getResp.StatusCode != http.StatusOK {
		log.Printf("GitHub fetch file info failed (status: %d): %v", getResp.StatusCode, err)
		http.Error(w, "Failed to retrieve current menu state from GitHub", http.StatusBadGateway)
		return
	}

	var getResult struct {
		Sha string `json:"sha"`
	}
	json.NewDecoder(getResp.Body).Decode(&getResult)
	getResp.Body.Close()

	// 2. Format JSON and encode to Base64
	formattedJSON, err := json.MarshalIndent(menuData, "", "  ")
	if err != nil {
		http.Error(w, "Failed to serialize JSON for commit", http.StatusInternalServerError)
		return
	}
	// Append newline to match standard POSIX file conventions
	formattedJSON = append(formattedJSON, '\n')
	encodedContent := base64.StdEncoding.EncodeToString(formattedJSON)

	// 3. Send commit PUT request to GitHub
	putURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, path)
	putPayload := map[string]string{
		"message": "chore(menu): update navigation configuration from admin dashboard",
		"content": encodedContent,
		"sha":     getResult.Sha,
		"branch":  branch,
	}
	putBody, _ := json.Marshal(putPayload)

	putReq, err := http.NewRequestWithContext(r.Context(), "PUT", putURL, bytes.NewBuffer(putBody))
	if err != nil {
		http.Error(w, "Failed to prepare commit request", http.StatusInternalServerError)
		return
	}
	putReq.Header.Set("Authorization", "Bearer "+token)
	putReq.Header.Set("Accept", "application/vnd.github.v3+json")
	putReq.Header.Set("Content-Type", "application/json")

	putResp, err := httpClient.Do(putReq)
	if err != nil || (putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated) {
		log.Printf("GitHub commit failed (status: %d): %v", putResp.StatusCode, err)
		http.Error(w, "Failed to commit changes to GitHub repository", http.StatusBadGateway)
		return
	}
	putResp.Body.Close()

	log.Printf("[ADMIN] Successfully committed updated menu.json to GitHub on branch %s", branch)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "Menu committed successfully. Build triggered on Cloudflare.",
	})
}

// catalogHandler streams the compressed catalog.json.gz with 304 ETag revalidation.
func catalogHandler(w http.ResponseWriter, r *http.Request) {
	//log.Println("called catalogHandler")
	clientVersion := r.URL.Query().Get("version")
	//fmt.Printf("catalogHandler - clientVersion: %s\n", clientVersion)
	ctx := r.Context()

	// 1. Ask R2 for the current ETag (The Source of Truth)
	head, err := storageClient.GetMetadata(ctx, "catalog.json.gz")
	if err != nil {
		http.Error(w, "Failed to check storage metadata", http.StatusInternalServerError)
		log.Printf("Error heading catalog: %v", err)
		return
	}
	r2ETag := *head.ETag
	//fmt.Printf("catalogHandler - r2ETag: %s\n", r2ETag)
	// 2. If client version matches R2 ETag, they are up to date!
	if clientVersion == r2ETag {
		//fmt.Println("catalogHandler - client is up to date")
		w.WriteHeader(http.StatusNotModified) // 304
		return
	}

	// 3. Client needs an update. Let's check our Go Server Cache.
	cachedETag, cachedBytes := catalogCache.Get()
	// 4. If Go Cache is stale or empty, fetch fresh bytes from R2
	if cachedETag != r2ETag || len(cachedBytes) == 0 {
		//log.Println("Go Server Cache Miss. Fetching catalog from R2...")
		freshBytes, err := storageClient.GetObjectBytes(ctx, "catalog.json.gz")
		if err != nil {
			http.Error(w, "Failed to fetch catalog from R2", http.StatusInternalServerError)
			return
		}
		// Update the Go Server RAM Cache
		catalogCache.Update(r2ETag, freshBytes)
		cachedETag = r2ETag
		cachedBytes = freshBytes
	}

	// 5. Stream the compressed bytes to the client
	// CHANGED: Tell the browser it's a binary blob, NOT auto-decompressing gzip
	w.Header().Set("Content-Type", "application/octet-stream")
	// REMOVED: w.Header().Set("Content-Encoding", "gzip")
	// Tell the browser it's JSON, but it's Gzipped (Browser will auto-decompress!)
	//w.Header().Set("Content-Type", "application/json")
	//w.Header().Set("Content-Encoding", "gzip")

	w.Header().Set("ETag", cachedETag) // Send the ETag so the client can save it
	w.Write(cachedBytes)

}

// --- TRANSPORT LAYER (HTTP Handlers) ---

// downloadHandler generates short-lived presigned URLs for track audio and logs stats.
func downloadHandler(w http.ResponseWriter, r *http.Request) {
	fileName := r.URL.Query().Get("file")
	if fileName == "" {
		http.Error(w, "Missing 'file' query parameter", http.StatusBadRequest)
		return
	}

	// Ask the library for the URL
	url, err := storageClient.GetDownloadURL(r.Context(), fileName, 5*time.Minute)
	if err != nil {
		// Note: The library automatically skips .json files and folders
		http.Error(w, "Failed to generate signed URL", http.StatusInternalServerError)
		log.Printf("Error signing downloadURL: %v", err)
		return
	}

	// Usage statistics: what was requested, from where, when. The primary
	// public-usage signal. Catalog fetches are deliberately NOT logged —
	// they fire on every page load and would be pure noise.
	stats.Record(Event{Kind: "track_download", File: fileName, IP: clientIP(r)})

	response := map[string]string{"url": url}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Limit upload size to 50MB to protect your server
	r.ParseMultipartForm(50 << 20) // 50MB limit

	// 2. Get the file from the form data (key name: "file")
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Failed to get file from request", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 3. Get metadata from form data
	title := r.FormValue("title")
	artist := r.FormValue("artist")
	audioHash := r.FormValue("audio-hash")

	metadata := map[string]string{
		"title":      title,
		"artist":     artist,
		"audio-hash": audioHash, // Saves the fingerprint to R2
	}

	// 4. Call the library to upload!
	err = storageClient.UploadFile(r.Context(), header.Filename, header.Size, file, metadata)
	if err != nil {
		http.Error(w, "Failed to upload file to R2", http.StatusInternalServerError)
		log.Printf("Error uploading: %v", err)
		return
	}

	//5. Return success
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "success", "file": header.Filename})
}

// updateMetadataHandler processes a request from the admin UI to overwrite
// the custom metadata for a specific track in R2.
// Because R2 does not allow patching metadata directly, this triggers a
// server-side "Copy-Over-Self" operation via the storage client.

func updateMetadataHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req MetadataUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Filename == "" {
		http.Error(w, "Filename is required", http.StatusBadRequest)
		return
	}

	// Schema filtering boundary
	// ─── INSERT 1: SCHEMA ENFORCEMENT (the boundary of truth) ───
	// Filter the incoming metadata against CatalogSchema BEFORE it reaches
	// either write path below. This single filter covers both consumers of
	// req.Metadata: the R2 Copy-Over-Self update (step 1) AND the
	// catalog.json.gz hot-patch (step F). Any field not defined in
	// schema.go — runtime fields like position/isActive/url sent by older
	// editor versions, or anything a crafted request injects — is
	// silently dropped here and can never reach R2 or the catalog.
	// (This was the primary pollution vector: step F previously wrote
	// every incoming key straight into catalog.json.gz, which every
	// visitor downloads.)
	//
	// NOTE: the storage layer additionally filters the MERGED metadata
	// (purging historical pollution already stored in R2); this filter
	// stops NEW pollution early, before any R2 I/O is spent on it.
	filtered := make(map[string]string, len(req.Metadata))
	for _, field := range schema.CatalogSchema {
		if v, ok := req.Metadata[field]; ok {
			filtered[field] = v
		}
	}
	req.Metadata = filtered

	ctx := r.Context()

	// 1. Update the MP3's metadata in R2
	err := storageClient.UpdateMetadata(ctx, req.Filename, req.Metadata)
	if err != nil {
		log.Printf("Failed to update metadata for %s: %v", req.Filename, err)
		http.Error(w, "Failed to update R2 metadata", http.StatusInternalServerError)
		return
	}

	// 2. HOT PATCH: Ensure catalog.json stays in sync

	// A. Check the current R2 ETag vs our RAM Cache ETag
	r2Head, err := storageClient.GetMetadata(ctx, "catalog.json.gz")
	if err != nil {
		log.Printf("Warning: Could not check R2 catalog ETag: %v", err)
		catalogCache.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		return
	}
	r2Etag := *r2Head.ETag

	var catalogGzBytes []byte
	ramEtag, ramBytes := catalogCache.Get()

	if ramEtag == r2Etag && len(ramBytes) > 0 {
		// B. HASHES MATCH: RAM is fresh! Use the RAM bytes (saves downloading the whole file)
		catalogGzBytes = ramBytes
	} else {
		// C. HASHES DO NOT MATCH: RAM is stale. Fetch the absolute latest from R2.
		log.Println("Cache stale during admin edit. Fetching fresh catalog from R2.")
		freshBytes, err := storageClient.GetObjectBytes(ctx, "catalog.json.gz")
		if err != nil {
			log.Printf("Warning: Could not fetch fresh catalog: %v", err)
			catalogCache.Clear()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "success"})
			return
		}
		catalogGzBytes = freshBytes
	}

	// D. Decompress the chosen bytes
	gzReader, err := gzip.NewReader(bytes.NewReader(catalogGzBytes))
	if err == nil {
		jsonBytes, err := io.ReadAll(gzReader)
		gzReader.Close()

		if err == nil {
			// E. Unmarshal into dynamic map
			var catalogData map[string]interface{}
			if json.Unmarshal(jsonBytes, &catalogData) == nil {

				// F. Find the track and update its fields
				if tracks, ok := catalogData["tracks"].([]interface{}); ok {
					for _, t := range tracks {
						if trackMap, ok := t.(map[string]interface{}); ok {
							if trackMap["filename"] == req.Filename {
								// ─── INSERT 2: SCHEMA PURGE (self-heal) ───
								// Remove any non-schema fields from this track
								// before applying the update. Historical pollution
								// (runtime fields written by older editor versions)
								// is cleaned permanently: each admin edit of a track
								// also heals its catalog entry. Safe for the catalog:
								// 'filename' and 'hash' ARE in CatalogSchema, so the
								// catalog keeps its keys — anything else on this
								// track was pollution by definition.
								for k := range trackMap {
									if !slices.Contains(schema.CatalogSchema, k) {
										delete(trackMap, k)
									}
								}

								for key, value := range req.Metadata {
									trackMap[key] = value
								}
								break
							}
						}
					}
				}

				// G. Re-marshal to JSON
				newJsonBytes, err := json.Marshal(catalogData)
				if err == nil {
					// H. Re-compress to Gzip
					var buf bytes.Buffer
					gzWriter := gzip.NewWriter(&buf)
					gzWriter.Write(newJsonBytes)
					gzWriter.Close()
					newGzBytes := buf.Bytes()

					// I. Upload the freshly patched catalog.json.gz back to R2
					err := storageClient.PutObjectBytes(ctx, "catalog.json.gz", newGzBytes)
					if err != nil {
						log.Printf("Warning: Failed to upload patched catalog to R2: %v", err)
					}

					// J. Update the RAM cache with the new bytes and fetch the NEW R2 ETag
					newHead, _ := storageClient.GetMetadata(ctx, "catalog.json.gz")
					newEtag := ""
					if newHead != nil && newHead.ETag != nil {
						newEtag = *newHead.ETag
					}
					catalogCache.Update(newEtag, newGzBytes)
				}
			}
		}
	}

	// 3. Return Success
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

// deleteTrackHandler deletes a single track from R2 and hot‑patches catalog.json.gz.
func deleteTrackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Filename == "" {
		http.Error(w, "Filename is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// 1. Delete the object from R2
	err := storageClient.DeleteObject(ctx, req.Filename)
	if err != nil {
		log.Printf("Failed to delete %s from R2: %v", req.Filename, err)
		http.Error(w, "Failed to delete from R2", http.StatusInternalServerError)
		return
	}

	// 2. Hot‑patch catalog.json.gz – remove the entry
	r2Head, err := storageClient.GetMetadata(ctx, "catalog.json.gz")
	if err != nil {
		log.Printf("Warning: Could not check catalog ETag after delete: %v", err)
		catalogCache.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		return
	}
	r2Etag := *r2Head.ETag

	var catalogGzBytes []byte
	ramEtag, ramBytes := catalogCache.Get()

	if ramEtag == r2Etag && len(ramBytes) > 0 {
		catalogGzBytes = ramBytes
	} else {
		log.Println("Cache stale during delete. Fetching fresh catalog from R2.")
		freshBytes, err := storageClient.GetObjectBytes(ctx, "catalog.json.gz")
		if err != nil {
			log.Printf("Warning: Could not fetch fresh catalog: %v", err)
			catalogCache.Clear()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "success"})
			return
		}
		catalogGzBytes = freshBytes
	}

	// Decompress
	gzReader, err := gzip.NewReader(bytes.NewReader(catalogGzBytes))
	if err != nil {
		log.Printf("Warning: gzip error: %v", err)
		catalogCache.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		return
	}
	jsonBytes, err := io.ReadAll(gzReader)
	gzReader.Close()
	if err != nil {
		log.Printf("Warning: failed to read decompressed catalog: %v", err)
		catalogCache.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		return
	}

	var catalogData map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &catalogData); err != nil {
		log.Printf("Warning: JSON unmarshal error: %v", err)
		catalogCache.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		return
	}

	// Remove the track with matching filename
	if tracks, ok := catalogData["tracks"].([]interface{}); ok {
		updatedTracks := make([]interface{}, 0, len(tracks))
		for _, t := range tracks {
			if trackMap, ok := t.(map[string]interface{}); ok {
				if trackMap["filename"] == req.Filename {
					continue // skip this one – it's being deleted
				}
			}
			updatedTracks = append(updatedTracks, t)
		}
		catalogData["tracks"] = updatedTracks
		catalogData["count"] = len(updatedTracks)
	}

	// Re‑marshal and re‑compress
	newJsonBytes, err := json.Marshal(catalogData)
	if err != nil {
		log.Printf("Warning: marshal error after delete: %v", err)
		catalogCache.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		return
	}
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	gzWriter.Write(newJsonBytes)
	gzWriter.Close()
	newGzBytes := buf.Bytes()

	// Upload updated catalog and update RAM cache
	err = storageClient.PutObjectBytes(ctx, "catalog.json.gz", newGzBytes)
	if err != nil {
		log.Printf("Warning: Failed to upload patched catalog: %v", err)
	}
	newHead, _ := storageClient.GetMetadata(ctx, "catalog.json.gz")
	newEtag := ""
	if newHead != nil && newHead.ETag != nil {
		newEtag = *newHead.ETag
	}
	catalogCache.Update(newEtag, newGzBytes)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

// UploadTrackRequest defines the metadata payload sent alongside the file - defined at the top for reuse in both the uploadTrackHandler and updateMetadataHandler
// This handler is designed for an admin tool that allows uploading a new track
//
//	along with its metadata in one request.
func uploadTrackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// 1. Parse the multipart form (max 50MB in memory)
	err := r.ParseMultipartForm(50 << 20)
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	// 2. Retrieve the file from the form
	file, _, err := r.FormFile("audioFile")
	if err != nil {
		http.Error(w, "Missing audio file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read the file into a byte slice (for an admin tool, buffering up to 50MB in RAM is safe)
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Error reading file", http.StatusInternalServerError)
		return
	}

	// 3. Retrieve the metadata JSON string from the form
	metadataStr := r.FormValue("metadata")
	var req UploadTrackRequest
	if err := json.Unmarshal([]byte(metadataStr), &req); err != nil {
		http.Error(w, "Invalid metadata JSON", http.StatusBadRequest)
		return
	}

	// ─── INSERT: SCHEMA ENFORCEMENT ───
	filtered := make(map[string]string, len(req.Metadata))
	for _, field := range schema.CatalogSchema {
		if v, ok := req.Metadata[field]; ok {
			filtered[field] = v
		}
	}
	req.Metadata = filtered
	// ─── END INSERT ───

	// 4. Upload the file to R2 with custom metadata
	err = storageClient.PutObjectWithMetadata(ctx, req.Filename, fileBytes, req.Metadata)
	if err != nil {
		log.Printf("Failed to upload file to R2: %v", err)
		http.Error(w, "Failed to upload to R2", http.StatusInternalServerError)
		return
	}

	// 5. Get the R2 ETag (MD5 Hash) of the newly uploaded file
	head, err := storageClient.GetMetadata(ctx, req.Filename)
	if err != nil {
		log.Printf("Warning: Failed to get ETag for new file: %v", err)
	}
	r2Hash := ""
	if head != nil {
		r2Hash = strings.Trim(*head.ETag, `"`) // R2 wraps MD5 in quotes
	}

	// 6. HOT PATCH: Append the new track to catalog.json.gz
	r2CatalogHead, _ := storageClient.GetMetadata(ctx, "catalog.json.gz")
	r2CatalogEtag := ""
	if r2CatalogHead != nil && r2CatalogHead.ETag != nil {
		r2CatalogEtag = *r2CatalogHead.ETag
	}

	ramEtag, ramBytes := catalogCache.Get()
	var catalogGzBytes []byte

	if ramEtag == r2CatalogEtag && len(ramBytes) > 0 {
		catalogGzBytes = ramBytes
	} else {
		catalogGzBytes, _ = storageClient.GetObjectBytes(ctx, "catalog.json.gz")
	}

	// Decompress and Unmarshal
	gzReader, _ := gzip.NewReader(bytes.NewReader(catalogGzBytes))
	jsonBytes, _ := io.ReadAll(gzReader)
	gzReader.Close()

	var catalogData map[string]interface{}
	json.Unmarshal(jsonBytes, &catalogData)

	// Build the new track object dynamically
	newTrack := map[string]string{
		"id":       req.Filename,
		"filename": req.Filename,
		"hash":     r2Hash,
	}
	// Merge in the schema-driven metadata
	for k, v := range req.Metadata {
		newTrack[k] = v
	}

	// Append the new track to the tracks array
	if tracks, ok := catalogData["tracks"].([]interface{}); ok {
		catalogData["tracks"] = append(tracks, newTrack)
		catalogData["count"] = len(catalogData["tracks"].([]interface{}))
	}

	// Re-marshal and Re-compress
	newJsonBytes, _ := json.Marshal(catalogData)
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	gzWriter.Write(newJsonBytes)
	gzWriter.Close()
	newGzBytes := buf.Bytes()

	// Upload updated catalog to R2 and update RAM cache
	storageClient.PutObjectBytes(ctx, "catalog.json.gz", newGzBytes)
	newCatalogHead, _ := storageClient.GetMetadata(ctx, "catalog.json.gz")
	newCatalogEtag := ""
	if newCatalogHead != nil && newCatalogHead.ETag != nil {
		newCatalogEtag = *newCatalogHead.ETag
	}
	catalogCache.Update(newCatalogEtag, newGzBytes)

	// 7. Return Success
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "success", "hash": r2Hash})
}

func getSignedUploadURLHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		http.Error(w, "Filename required", http.StatusBadRequest)
		return
	}

	// Generate a URL that expires in 15 minutes (plenty of time for large file uploads)
	url, err := storageClient.GetUploadURL(r.Context(), filename, 15*time.Minute)
	if err != nil {
		http.Error(w, "Failed to generate upload URL", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}

// startCrawlWorker is the background Goroutine that does the heavy lifting
func startCrawlWorker(jobID string) {
	// 1. Setup the progress channel
	progressChan := make(chan storage.CrawlProgress, 10) // Buffered channel

	// 2. Goroutine to listen to progress and update the registry
	go func() {
		for progress := range progressChan {
			job, _ := jobRegistry.Load(jobID)
			j := job.(*CrawlJob)

			j.Progress = progress.Percent
			j.Message = progress.Message

			// Save updated job back to registry
			jobRegistry.Store(jobID, j)
		}
	}()

	// 3. Execute the actual crawl
	catalogData, err := storageClient.CrawlCatalog(context.Background(), progressChan)
	close(progressChan) // Signal the listener to stop

	// 4. Handle Completion / Failure
	job, _ := jobRegistry.Load(jobID)
	j := job.(*CrawlJob)

	if err != nil {
		j.Status = "failed"
		j.Message = err.Error()
	} else {
		// ==========================================
		// NEW: Compress and Upload to R2
		// ==========================================
		j.Message = "Compressing catalog..."
		jobRegistry.Store(jobID, j)

		jsonData, err := json.MarshalIndent(catalogData, "", "  ")
		if err != nil {
			j.Status = "failed"
			j.Message = "Failed to marshal JSON"
			jobRegistry.Store(jobID, j)
			return
		}

		var buf bytes.Buffer
		gzWriter := gzip.NewWriter(&buf)
		gzWriter.Name = "catalog.json"
		gzWriter.Write(jsonData)
		gzWriter.Close()
		gzBytes := buf.Bytes()

		j.Message = "Uploading catalog to R2..."
		jobRegistry.Store(jobID, j)

		err = storageClient.PutObjectBytes(context.Background(), "catalog.json.gz", gzBytes)
		if err != nil {
			j.Status = "failed"
			j.Message = "Failed to upload catalog to R2"
			jobRegistry.Store(jobID, j)
			return
		}

		// ==========================================
		// SUCCESS: Clear cache and finalize
		// ==========================================
		catalogCache.Clear() // Force the server to download the new one on next request

		j.Status = "completed"
		j.Progress = 100
		j.Message = "Catalog updated successfully."

	}

	jobRegistry.Store(jobID, j)
}

// 1. Trigger the Crawl
func startCrawlHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Generate a unique Job ID (simple timestamp + random for now)
	jobID := fmt.Sprintf("crawl-%d", time.Now().UnixNano())

	// Initialize the Job in the registry
	jobRegistry.Store(jobID, &CrawlJob{
		ID:       jobID,
		Status:   "running",
		Progress: 0,
		Message:  "Initializing crawl...",
	})

	// Spin up the background worker
	go startCrawlWorker(jobID)

	// Immediately return 202 Accepted + JobID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// 2. Check the Status
func crawlStatusHandler(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "Missing job_id", http.StatusBadRequest)
		return
	}

	job, exists := jobRegistry.Load(jobID)
	if !exists {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

// =============================================================================
// CORS MIDDLEWARE
// =============================================================================

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Define your allowed origins
		allowedOrigins := []string{
			"http://localhost:4321",        // Local Development
			"https://labriideas.pages.dev", // Production Cloudflare Site
		}

		// 2. Get the origin from the incoming request; if it is in our
		//    allow-list, reflect it back exactly. Unlisted origins get
		//    no ACAO header at all — the browser then blocks the
		//    response, which is the CORS layer doing its (limited) job.
		//    NOTE: CORS is a browser convenience, NOT the security
		//    boundary — authMiddleware is. curl ignores CORS entirely.
		origin := r.Header.Get("Origin")
		for _, allowed := range allowedOrigins {
			if origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				break
			}
		}

		// 3. Allow the methods we use
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		// 4. Allow request headers. CHANGED: added Authorization —
		//    REQUIRED for the Bearer-token auth overlay. It is a
		//    non-simple header, so any authed request triggers a
		//    preflight (OPTIONS); without it listed here the browser
		//    kills the request client-side (ERR_FAILED) and it never
		//    reaches authMiddleware at all. Content-Type remains for
		//    JSON bodies and multipart/form-data uploads.
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		// 5. Expose the ETag header so the frontend can read it (for caching)
		w.Header().Set("Access-Control-Expose-Headers", "ETag")

		// 6. Cache the preflight result for a day — the browser stops
		//    re-sending OPTIONS for repeated uploads/fetches to the
		//    same route. Free performance win, no behavior change.
		w.Header().Set("Access-Control-Max-Age", "86400")

		// 7. Intercept the Preflight OPTIONS request and return 204.
		//    Must stay BEFORE next(w, r): preflights never carry
		//    credentials by design, so authMiddleware would 401 them.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// 8. Pass normal requests to the actual handler
		next(w, r)
	}
}
