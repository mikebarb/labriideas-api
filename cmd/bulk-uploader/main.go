package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mikebarb/labriideas-publisher/pkg/client"
	"github.com/mikebarb/labriideas-publisher/pkg/csvcheck"
	"github.com/mikebarb/labriideas-publisher/pkg/csvparser"
	"github.com/mikebarb/labriideas-publisher/pkg/jobs"
	"github.com/mikebarb/labriideas-publisher/pkg/media"
)

func main() {
	const defaultLogDir = `C:\Users\Barbara\labri_other_files\logs`
	logPath := client.SetupLogging(defaultLogDir)
	if logPath != "" {
		fmt.Printf("📝 Logging to: %s\n", logPath)
	}

	log.Println("=== Labriideas Bulk Upload Tool ===")
	log.Println("Initializing...")

	// ==============================================
	// INPUT: csv file to read from for bulk delete
	//        location of audio mp3 files on disk
	// ==============================================
	csvPath := `C:\Users\Barbara\labri_other_files\enriched_catalog_matched.csv`
	audioDirPath := `C:\Users\Barbara\labri_audio_files`

	// ===================================================
	// Validate CSV file format BEFORE doing anything else
	// ===================================================
	csvcheck.MustCheck(csvPath)

	// ==============================================
	// SETUP: Determine dry-run and skip-crawl flags
	// ==============================================
	// Determine dry-run from CLI arg or env var
	isDryRun := false
	for _, arg := range os.Args[1:] {
		if arg == "--dry-run" {
			isDryRun = true
			break
		}
	}
	if !isDryRun && strings.ToLower(os.Getenv("DRY_RUN")) == "true" {
		isDryRun = true
	}

	// Determine skipCrawl from CLI arg or env var
	skipCrawl := false
	for _, arg := range os.Args[1:] {
		if arg == "--skip-crawl" {
			skipCrawl = true
			break
		}
	}
	if !skipCrawl && strings.ToLower(os.Getenv("SKIP_CRAWL")) == "true" {
		skipCrawl = true
	}

	c := client.NewFromEnv()

	// ==========================================
	// STEP prep 0a: Initial crawl
	// ==========================================
	if !skipCrawl {
		log.Println("\n--- Triggering Catalog Rebuild on Server - initial run ---")
		jobID1, err := c.TriggerCrawl()
		if err != nil {
			log.Fatalf("Failed to trigger crawl - initial run: %v", err)
		}
		client.Printf("Crawl Job Started (ID: %s). Waiting for server to rebuild catalog...\n", jobID1)
		if err := c.PollCrawlStatus(jobID1); err != nil {
			log.Fatalf("Crawl failed: %v", err)
		}
	} else {
		log.Println("\n--- Skipping initial crawl (SKIP_CRAWL=true) ---")
	}
	// ==========================================
	// STEP 1: Parse CSV & verify local files
	// ==========================================
	file, err := os.Open(csvPath)
	if err != nil {
		log.Fatalf("Error opening CSV: %v", err)
	}
	defer file.Close()

	records, err := csvparser.Parse(file)
	if err != nil {
		log.Fatalf("Error parsing CSV: %v", err)
	}
	client.Printf("\nSuccessfully parsed %d records from CSV.\n", len(records))

	var readyToProcess []csvparser.ParsedRecord
	var missingFiles []string
	for _, rec := range records {
		fullPath := filepath.Join(audioDirPath, rec.Filename)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			missingFiles = append(missingFiles, rec.Filename)
		} else {
			readyToProcess = append(readyToProcess, rec)
		}
	}
	if len(missingFiles) > 0 {
		client.Printf("\n⚠️ WARNING: %d files missing from disk:\n", len(missingFiles))
		for _, f := range missingFiles {
			client.Printf("  - %s\n", f)
		}
	}
	client.Printf("✅ %d files verified on disk.\n", len(readyToProcess))

	// ==========================================
	// STEP 2: Fetch catalog
	// ==========================================
	log.Println("\n--- Fetching current catalog from server ---")
	r2Catalog, r2HashCatalog, err := c.FetchCatalog()
	if err != nil {
		log.Fatalf("Failed to fetch catalog: %v", err)
	}
	client.Printf("✅ Loaded %d existing tracks from server.\n", len(r2Catalog))

	// ==========================================
	// STEP 3: Diff
	// ==========================================
	log.Println("\n--- Running Diffing Engine ---")
	var listUpload []csvparser.ParsedRecord
	var listUpdate []csvparser.ParsedRecord
	var listSkip []csvparser.ParsedRecord

	for _, localRec := range readyToProcess {
		r2Track, existsInR2 := r2Catalog[localRec.Filename]
		if !existsInR2 {
			localFilePath := filepath.Join(audioDirPath, localRec.Filename)
			audioHash, _ := media.CalculateAudioHash(localFilePath)
			if existingFile, isDuplicate := r2HashCatalog[audioHash]; isDuplicate {
				client.Printf("  ⚠️ DUPLICATE AUDIO: %s is identical to %s (Skipping)\n", localRec.Filename, existingFile)
				continue
			}
			localRec.Metadata["audio-hash"] = audioHash
			listUpload = append(listUpload, localRec)
		} else {
			if client.MetadataMatches(localRec.Metadata, r2Track) {
				listSkip = append(listSkip, localRec)
			} else {
				listUpdate = append(listUpdate, localRec)
			}
		}
	}

	log.Println("\n=== DIFFING RESULTS ===")
	client.Printf("🟢 NEW (To Upload):     %d files\n", len(listUpload))
	client.Printf("🟡 CHANGED (To Update): %d files\n", len(listUpdate))
	client.Printf("⚪ UNCHANGED (Skip):    %d files\n", len(listSkip))

	// ==========================================
	// Dry-run preview (consistent with deleter)
	// ==========================================
	if isDryRun {
		log.Println("\n--- DRY RUN – no uploads or updates will be performed ---")
		if len(listUpload) > 0 {
			client.Printf("Would upload %d file(s):\n", len(listUpload))
			for _, rec := range listUpload {
				client.Printf("  + %s\n", rec.Filename)
			}
		}
		if len(listUpdate) > 0 {
			client.Printf("Would update metadata for %d file(s):\n", len(listUpdate))
			for _, rec := range listUpdate {
				client.Printf("  ~ %s\n", rec.Filename)
			}
		}
		if len(listUpload) == 0 && len(listUpdate) == 0 {
			log.Println("No changes would be made.")
		}
		return
	}

	// ==========================================
	// STEP 4.0: Confirmation prompt
	// ==========================================
	if !(len(listUpload) == 0 && len(listUpdate) == 0) {
		fmt.Printf("\nProceed with %d uploads and %d updates? [y/N]: ", len(listUpload), len(listUpdate))
		var input string
		fmt.Scanln(&input)
		if strings.ToLower(strings.TrimSpace(input)) != "y" {
			fmt.Println("Aborted by user.")
			return
		}
	}

	// ==========================================
	// STEP 4: Execute uploads & updates (with bounded concurrency)
	// ==========================================
	successCount := 0
	var failed []string
	var mu sync.Mutex

	concurrency := 4
	if v := os.Getenv("UPLOAD_CONCURRENCY"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			concurrency = n
		}
	}

	// 4A: Uploads
	if len(listUpload) > 0 {
		log.Println("\n--- Uploading New Files to R2 ---")
		jobs.RunParallel(listUpload, concurrency,
			func(rec csvparser.ParsedRecord) error {
				return uploadViaPresignedURL(c, rec, audioDirPath)
			},
			func(rec csvparser.ParsedRecord) {
				mu.Lock()
				successCount++
				mu.Unlock()
			},
			func(rec csvparser.ParsedRecord, err error) {
				mu.Lock()
				failed = append(failed, rec.Filename)
				mu.Unlock()
				client.Printf("  ❌ Upload Failed: %s (%v)\n", rec.Filename, err)
			},
		)
	}

	// 4B: Metadata updates
	if len(listUpdate) > 0 {
		log.Println("\n--- Updating Metadata on Existing Files ---")
		jobs.RunParallel(listUpdate, concurrency,
			func(rec csvparser.ParsedRecord) error {
				return c.UpdateMetadata(rec.Filename, rec.Metadata)
			},
			func(rec csvparser.ParsedRecord) {
				mu.Lock()
				successCount++
				mu.Unlock()
				client.Printf("  ✅ Updated: %s\n", rec.Filename)
			},
			func(rec csvparser.ParsedRecord, err error) {
				mu.Lock()
				failed = append(failed, rec.Filename)
				mu.Unlock()
				client.Printf("  ❌ Metadata Update Failed: %s (%v)\n", rec.Filename, err)
			},
		)
	}

	if successCount == 0 && len(failed) == 0 {
		log.Println("\nNo changes made. Exiting.")
		return
	}

	// ==========================================
	// STEP 5: Final crawl
	// ==========================================
	log.Println("\n--- Triggering Catalog Rebuild on Server ---")
	jobID2, err := c.TriggerCrawl()
	if err != nil {
		log.Fatalf("Failed to trigger crawl: %v", err)
	}
	client.Printf("Crawl Job Started (ID: %s). Waiting for server to rebuild catalog...\n", jobID2)
	if err := c.PollCrawlStatus(jobID2); err != nil {
		log.Fatalf("Crawl failed: %v", err)
	}

	log.Println("\n=== Bulk Upload Complete! ===")
	if len(failed) > 0 {
		client.Printf("❌ Failed: %d\n", len(failed))
		for _, f := range failed {
			client.Printf("    - %s\n", f)
		}
	}
}

// uploadViaPresignedURL uploads one file via a presigned URL, then updates metadata.
func uploadViaPresignedURL(c *client.Client, rec csvparser.ParsedRecord, audioDirPath string) error {
	url, err := c.GetSignedUploadURL(rec.Filename)
	if err != nil {
		return err
	}

	filePath := filepath.Join(audioDirPath, rec.Filename)
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}

	req, err := http.NewRequest("PUT", url, file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "audio/mpeg")
	req.ContentLength = stat.Size()

	r2Resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error on upload: %w", err)
	}
	defer r2Resp.Body.Close()

	if r2Resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(r2Resp.Body)
		return fmt.Errorf("R2 rejected upload (Status %d): %s", r2Resp.StatusCode, string(bodyBytes))
	}

	if err := c.UpdateMetadata(rec.Filename, rec.Metadata); err != nil {
		return fmt.Errorf("file uploaded but metadata failed: %w", err)
	}
	client.Printf("  ✅ Uploaded: %s\n", rec.Filename)
	return nil
}
