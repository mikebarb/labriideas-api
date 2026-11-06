package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mikebarb/labriideas-publisher/pkg/client"
	"github.com/mikebarb/labriideas-publisher/pkg/csvio"
)

func main() {
	fmt.Println("=== Labriideas Catalog Exporter ===")
	fmt.Println("Initializing...")

	// ==========================================
	// Flags
	// ==========================================
	//isDryRun := client.IsDryRun()
	//skipCrawl := client.IsCrawlSkipped()

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

	// ==========================================
	// Setup logging
	// ==========================================
	logPath := client.SetupLogging("")
	if logPath != "" {
		client.Printf("📝 Logging to: %s", logPath)
	}

	c := client.NewFromEnv()

	// ==========================================
	// STEP 0: Authenticate (admin framework)
	// ==========================================
	// Every admin endpoint this tool uses — crawl, presigned upload URL,
	// metadata update — now requires a Bearer credential. The client resolves
	// one in this order (see pkg/client/auth.go):
	//   1. PUBLISHER_API_TOKEN   (static token, unattended runs)
	//   2. ADMIN_API_TOKENS      (first uid:secret, shared with the server .env)
	//   3. ADMIN_PASSWORDS       (first uid:secret, exchanged for a session)
	//   4. Interactive no-echo password prompt
	//
	// SKIPPED only for a dry run that also skips crawling: that path calls
	// nothing but the PUBLIC /api/catalog endpoint.
	needAuth := !(isDryRun && skipCrawl)
	if needAuth {
		if err := c.EnsureAuthenticated(); err != nil {
			log.Fatalf("Authentication failed: %v", err)
		}
		// Best effort: log.Fatalf calls os.Exit and skips this, in which case
		// the server session simply expires on its own.
		defer c.Logout()
	}

	client.Printf("Target: %s (%s)", c.BaseURL, client.EnvironmentLabel)
	if needAuth {
		client.Printf("%s", c.Identity())
	}

	// Output directory — from env or default
	outputDir := os.Getenv("CATALOG_EXPORT_DIR")
	if outputDir == "" {
		outputDir = `C:\Users\Barbara\labri_other_files`
	}

	// ==========================================
	// Optional initial crawl
	// ==========================================
	if !skipCrawl {
		client.Print("--- Triggering Catalog Rebuild on Server ---")
		jobID, err := c.TriggerCrawl()
		if err != nil {
			log.Fatalf("Failed to trigger crawl: %v", err)
		}
		client.Printf("Crawl Job Started (ID: %s)...", jobID)
		if err := c.PollCrawlStatus(jobID); err != nil {
			log.Fatalf("Crawl failed: %v", err)
		}
	} else {
		client.Print("--- Skipping initial crawl (SKIP_CRAWL=true) ---")
	}

	// ==========================================
	// Fetch catalog
	// ==========================================
	client.Print("--- Fetching current catalog from server ---")
	catalogMap, _, err := c.FetchCatalog()
	if err != nil {
		log.Fatalf("Failed to fetch catalog: %v", err)
	}
	client.Printf("✅ Loaded %d tracks from server.", len(catalogMap))

	if len(catalogMap) == 0 {
		client.Print("⚠️  Catalog is empty. Nothing to export.")
		return
	}

	// ==========================================
	// Build output path
	// ==========================================
	ts := time.Now().Format("2006-01-02_15-04-05")
	outputName := fmt.Sprintf("catalog_export_%s.csv", ts)
	outputPath := filepath.Join(outputDir, outputName)

	// ==========================================
	// Dry-run preview
	// ==========================================
	if isDryRun {
		client.Print("--- DRY RUN – no file will be written ---")
		client.Printf("Would export %d tracks to: %s", len(catalogMap), outputPath)
		return
	}

	// ==========================================
	// Ensure output directory exists
	// ==========================================
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		log.Fatalf("Could not create output directory %q: %v", outputDir, err)
	}

	// ==========================================
	// Write CSV
	// ==========================================
	client.Printf("--- Writing export to %s ---", outputPath)
	rowCount, err := csvio.WriteCatalogCSV(outputPath, catalogMap)
	if err != nil {
		log.Fatalf("Failed to write output CSV: %v", err)
	}

	// ==========================================
	// Summary
	// ==========================================
	client.Printf("✅ Exported %d tracks.", rowCount)
	client.Printf("✅ Output: %s", outputPath)
}
