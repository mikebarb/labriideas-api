package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"

	"github.com/mikebarb/labriideas-publisher/pkg/cache"
	"github.com/mikebarb/labriideas-publisher/pkg/storage"
)

//"io"

// Global storage client
var storageClient *storage.Client
var catalogCache *cache.CatalogCache

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

// In-memory store for job statuses
var jobRegistry sync.Map

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, relying on system env vars")
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
	mux.HandleFunc("/api/download", corsMiddleware(downloadHandler))
	mux.HandleFunc("/api/upload", corsMiddleware(uploadHandler))
	mux.HandleFunc("/api/catalog", corsMiddleware(catalogHandler))
	mux.HandleFunc("/api/update-metadata", corsMiddleware(updateMetadataHandler))
	mux.HandleFunc("/api/upload-track", corsMiddleware(uploadTrackHandler))
	mux.HandleFunc("/api/get-upload-url", corsMiddleware(getSignedUploadURLHandler))
	mux.HandleFunc("/api/start-crawl", corsMiddleware(startCrawlHandler))
	mux.HandleFunc("/api/crawl-status", corsMiddleware(crawlStatusHandler))
	mux.HandleFunc("/api/delete-track", corsMiddleware(deleteTrackHandler))

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

	// 3. Log the correct local URL
	log.Printf("🚀 Server starting on http://localhost:%s", port)

	//log.Println("🚀 Server starting on http://localhost:8080")
	//log.Fatal(http.ListenAndServe(":8080", mux))

	// 4. Start the server using the dynamic port
	// Notice we use ":" + port, not ":8080"
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}

}

// --- CATALOG HANDLER ---
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
			http.Error(w, "Failed to fetch catalog", http.StatusInternalServerError)
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
		log.Printf("Error signing URL: %v", err)
		return
	}

	response := map[string]string{"url": url}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Limit upload size to 50MB to protect your server
	r.ParseMultipartForm(50 << 20)

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
					if newHead != nil {
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
	if newHead != nil {
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
	if r2CatalogHead != nil {
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
	if newCatalogHead != nil {
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
		Message:  "Initializing...",
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

// --- MIDDLEWARE ---

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	log.Println("corsMiddleware called.")
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Allow requests from your Astro frontend
		//w.Header().Set("Access-Control-Allow-Origin", "*")
		//w.Header().Set("Access-Control-Allow-Origin", "https://labriideas.pages.dev")
		//w.Header().Set("Access-Control-Allow-Origin", "http://localhost:4321")

		// 1. Define your allowed origins
		allowedOrigins := []string{
			"http://localhost:4321",        // Local Development
			"https://labriideas.pages.dev", // Production Cloudflare Site
		}

		// 2. Get the origin from the incoming request
		origin := r.Header.Get("Origin")

		// 3. If the origin is in our allowed list, reflect it back exactly
		for _, allowed := range allowedOrigins {
			if origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				break
			}
		}

		// 2. Allow the methods we use
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		// 3. Allow the Content-Type header (Crucial for multipart/form-data)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// 4. Expose the ETag header so the frontend can read it (for caching)
		w.Header().Set("Access-Control-Expose-Headers", "ETag")

		// 5. Intercept the Preflight OPTIONS request and return 204 No Content
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// 6. Pass normal requests to the actual handler
		next(w, r)
	}
}
