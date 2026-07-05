package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/mikebarb/labriideas-publisher/pkg/client"
	"github.com/mikebarb/labriideas-publisher/pkg/csvparser"
	"github.com/mikebarb/labriideas-publisher/pkg/jobs"
)

func main() {
	fmt.Println("=== Labriideas Bulk Delete Tool ===")
	fmt.Println("Initializing...")

	// ==============================================
	// INPUT: csv file to read from for bulk delete
	// ==============================================
	csvPath := `C:\Users\Barbara\labri_other_files\catalog_export_utf8.csv`

	// ===================================================
	// Validate CSV file format BEFORE doing anything else
	// ===================================================
	//csvcheck.MustCheck(csvPath)

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
		fmt.Println("\n--- Triggering Catalog Rebuild on Server (initial) ---")
		jobID1, err := c.TriggerCrawl()
		if err != nil {
			log.Fatalf("Failed to trigger crawl: %v", err)
		}
		fmt.Printf("Crawl Job Started (ID: %s). Waiting for server to rebuild catalog...\n", jobID1)
		if err := c.PollCrawlStatus(jobID1); err != nil {
			log.Fatalf("Crawl failed: %v", err)
		}
	} else {
		fmt.Println("\n--- Skipping initial crawl (SKIP_CRAWL=true) ---")
	}

	// ==========================================
	// STEP 1: Parse CSV
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
	fmt.Printf("\nSuccessfully parsed %d records from CSV.\n", len(records))

	csvFilenames := make(map[string]struct{})
	for _, rec := range records {
		if rec.Filename != "" {
			csvFilenames[rec.Filename] = struct{}{}
		}
	}
	if len(csvFilenames) == 0 {
		log.Fatalln("No filenames found in CSV – nothing to delete.")
	}

	// ==========================================
	// STEP 2: Fetch catalog
	// ==========================================
	fmt.Println("\n--- Fetching current catalog from server ---")
	catalogMap, _, err := c.FetchCatalog()
	if err != nil {
		log.Fatalf("Failed to fetch catalog: %v", err)
	}
	fmt.Printf("✅ Loaded %d existing tracks from server.\n", len(catalogMap))

	// ==========================================
	// STEP 3: Diff
	// ==========================================
	fmt.Println("\n--- Checking which files exist in R2 ---")
	var toDelete []string
	var notInCatalog []string
	for fn := range csvFilenames {
		if _, exists := catalogMap[fn]; exists {
			toDelete = append(toDelete, fn)
		} else {
			notInCatalog = append(notInCatalog, fn)
		}
	}

	fmt.Printf("🔴 To delete from R2:     %d\n", len(toDelete))
	fmt.Printf("⚠️  Not in catalog (skip): %d\n", len(notInCatalog))
	if len(notInCatalog) > 0 {
		fmt.Println("  Skipping (not in R2):")
		for _, fn := range notInCatalog {
			fmt.Printf("    - %s\n", fn)
		}
	}
	if len(toDelete) == 0 {
		fmt.Println("\nNo files to delete. Exiting.")
		return
	}

	// ==========================================
	// STEP 3.5: Dry-run preview
	// ==========================================
	if isDryRun {
		fmt.Println("\n--- DRY RUN – no deletions will be performed ---")
		fmt.Printf("Would delete %d file(s):\n", len(toDelete))
		for _, fn := range toDelete {
			fmt.Printf("  - %s\n", fn)
		}
		return
	}

	// ==========================================
	// STEP 3.6: Confirmation prompt
	// ==========================================
	fmt.Printf("\nProceed with %d deletions? [y/N]: ", len(toDelete))
	var input string
	fmt.Scanln(&input)
	if strings.ToLower(strings.TrimSpace(input)) != "y" {
		fmt.Println("Aborted by user.")
		return
	}

	// ==========================================
	// STEP 4: Delete with bounded concurrency
	// ==========================================
	fmt.Println("\n--- Deleting files from R2 ---")
	concurrency := 4
	if v := os.Getenv("DEL_CONCURRENCY"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			concurrency = n
		}
	}

	var successCount int
	var failed []struct {
		Filename string
		Error    string
	}
	var mu sync.Mutex

	jobs.RunParallel(toDelete, concurrency,
		func(fn string) error {
			return c.DeleteTrack(fn)
		},
		func(fn string) {
			mu.Lock()
			successCount++
			mu.Unlock()
			fmt.Printf("  ✅ Deleted: %s\n", fn)
		},
		func(fn string, err error) {
			mu.Lock()
			failed = append(failed, struct {
				Filename string
				Error    string
			}{fn, err.Error()})
			mu.Unlock()
			fmt.Printf("  ❌ Failed: %s (%v)\n", fn, err)
		},
	)

	if successCount == 0 && len(failed) == 0 {
		fmt.Println("\nNo changes made. Exiting.")
		return
	}

	// ==========================================
	// STEP 5: Final crawl
	// ==========================================
	if !isDryRun {
		fmt.Println("\n--- Triggering final Catalog Rebuild on Server ---")
		jobID2, err := c.TriggerCrawl()
		if err != nil {
			log.Fatalf("Failed to trigger final crawl: %v", err)
		}
		fmt.Printf("Crawl Job Started (ID: %s). Waiting for server to rebuild catalog...\n", jobID2)
		if err := c.PollCrawlStatus(jobID2); err != nil {
			log.Fatalf("Final crawl failed: %v", err)
		}
	} else {
		fmt.Println("\n--- Skipping final crawl (DRY_RUN=true) ---")
	}
	// ==========================================
	// Summary
	// ==========================================
	fmt.Println("\n=== Bulk Delete Complete! ===")
	fmt.Printf("✅ Successfully deleted: %d\n", successCount)
	if len(failed) > 0 {
		fmt.Printf("❌ Failed deletions: %d\n", len(failed))
		for _, f := range failed {
			fmt.Printf("    - %s: %s\n", f.Filename, f.Error)
		}
	}
	if len(notInCatalog) > 0 {
		fmt.Printf("⚠️  Skipped (not in R2): %d\n", len(notInCatalog))
	}
}
