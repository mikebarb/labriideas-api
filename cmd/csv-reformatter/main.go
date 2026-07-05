package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/mikebarb/labriideas-publisher/pkg/client"
	"github.com/mikebarb/labriideas-publisher/pkg/csvcheck"
	"github.com/mikebarb/labriideas-publisher/pkg/csvio"
)

func main() {
	fmt.Println("=== Labriideas CSV Reformatter ===")
	fmt.Println("Initializing...")

	// ==========================================
	// Step 1: read --dry-run / --skip-crawl from os.Args BEFORE
	//         letting the standard flag package see them.
	// ==========================================
	isDryRun := client.IsDryRun()

	// ==========================================
	// Step 2: define -out using a private FlagSet (not the global one),
	//         then parse the remaining args. Because the FlagSet has no
	//         --dry-run / --skip-crawl defined, those flags pass through
	//         untouched and can be read by client.IsXxx() in any order.
	// ==========================================
	fs := flag.NewFlagSet("csv-reformatter", flag.ContinueOnError)
	var outputDir string
	fs.StringVar(&outputDir, "out", "", "Output directory (default: same as input file)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		// FlagSet already printed the error; just exit.
		os.Exit(2)
	}

	// ==========================================
	// Setup logging
	// ==========================================
	logPath := client.SetupLogging("")
	if logPath != "" {
		client.Printf("📝 Logging to: %s", logPath)
	}

	c := client.NewFromEnv()
	_ = c // local-only tool, kept for symmetry with other tools

	// ==========================================
	// Validate input file argument
	// ==========================================
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: csv-reformatter [-out <dir>] [--dry-run] <input.csv>")
		os.Exit(1)
	}
	inputPath := fs.Arg(0)
	client.Printf("Input file: %s", inputPath)

	// ==========================================
	// Validate the input CSV format (encoding)
	// ==========================================
	csvcheck.MustCheck(inputPath)

	// ==========================================
	// Read the input CSV
	// ==========================================
	client.Print("--- Reading input CSV ---")
	catalog, err := csvio.ReadCatalogCSV(inputPath)
	if err != nil {
		log.Fatalf("Failed to read input CSV: %v", err)
	}
	client.Printf("✅ Loaded %d rows from input.", len(catalog))

	if len(catalog) == 0 {
		client.Print("⚠️  Input CSV is empty. Nothing to reformat.")
		return
	}

	// ==========================================
	// Determine output path
	// ==========================================
	if outputDir == "" {
		outputDir = filepath.Dir(inputPath)
	}

	ts := time.Now().Format("2006-01-02_15-04-05")
	inputBase := filepath.Base(inputPath)
	inputExt := filepath.Ext(inputBase)
	inputStem := inputBase[:len(inputBase)-len(inputExt)]
	outputName := fmt.Sprintf("%s_reformatted_%s%s", inputStem, ts, inputExt)
	outputPath := filepath.Join(outputDir, outputName)

	// ==========================================
	// Dry-run preview
	// ==========================================
	if isDryRun {
		client.Print("--- DRY RUN – no file will be written ---")
		client.Printf("Would reformat %d rows to: %s", len(catalog), outputPath)
		return
	}

	// ==========================================
	// Ensure output directory exists
	// ==========================================
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		log.Fatalf("Could not create output directory %q: %v", outputDir, err)
	}

	// ==========================================
	// Write the reformatted CSV
	// ==========================================
	client.Printf("--- Writing reformatted CSV to %s ---", outputPath)
	rowCount, err := csvio.WriteCatalogCSV(outputPath, catalog)
	if err != nil {
		log.Fatalf("Failed to write output CSV: %v", err)
	}

	// ==========================================
	// Summary
	// ==========================================
	client.Printf("✅ Reformatted %d rows.", rowCount)
	client.Printf("✅ Output: %s", outputPath)
}
