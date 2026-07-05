package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dhowden/tag"
)

// CatalogEntry holds the original raw row plus our extracted matching keys
type CatalogEntry struct {
	OriginalRow []string
	Title       string
	Speaker     string
}

var nonAlphanumericRegex = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeAlphanumeric(s string) string {
	lower := strings.ToLower(s)
	return nonAlphanumericRegex.ReplaceAllString(lower, "")
}

// extractMP3Tags reads the ID3 tags from an audio file
func extractMP3Tags(audioDir string, filename string) (title, artist, album string) {
	fullPath := filepath.Join(audioDir, filename)
	file, err := os.Open(fullPath)
	if err != nil {
		return "", "", ""
	}
	defer file.Close()

	tags, err := tag.ReadFrom(file)
	if err != nil {
		return "", "", "" // Not an audio file or no tags
	}

	return tags.Title(), tags.Artist(), tags.Album()
}

func main() {
	fmt.Println("=== Catalog Matcher Tool ===")

	// --- CONFIGURATION ---
	catalogCSVPath := `C:\Users\Barbara\labri_other_files\IdeasLibraryDatabase.csv`
	audioDirPath := `C:\Users\Barbara\labri_audio_files`
	outputCSVPath := `C:\Users\Barbara\labri_other_files\enriched_catalog.csv`
	// ----------------------

	headers, catalogEntries, err := readCatalog(catalogCSVPath)
	if err != nil {
		fmt.Printf("Error reading catalog: %v\n", err)
		return
	}
	fmt.Printf("Loaded %d catalog entries.\n", len(catalogEntries))

	files, err := os.ReadDir(audioDirPath)
	if err != nil {
		fmt.Printf("Error reading audio directory: %v\n", err)
		return
	}

	outFile, err := os.Create(outputCSVPath)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		return
	}
	defer outFile.Close()

	writer := csv.NewWriter(outFile)
	defer writer.Flush()

	// ==========================================
	// OUTPUT HELPERS
	// ==========================================
	seqCounter := 0

	// Helper to write rows with guaranteed column layout
	writeOutputRow := func(originalRow []string, filename string, mp3Title string, mp3Artist string, mp3Album string, status string) {
		row := []string{fmt.Sprintf("%d", seqCounter)} // 1. Sequence Number

		// 2. Original Catalog Columns (pad with empty strings if row is missing)
		for i := 0; i < len(headers); i++ {
			if i < len(originalRow) {
				row = append(row, originalRow[i])
			} else {
				row = append(row, "")
			}
		}

		// 3. New Enrichment Columns
		row = append(row, filename)  // matched_filename
		row = append(row, mp3Title)  // mp3_title
		row = append(row, mp3Artist) // mp3_artist
		row = append(row, mp3Album)  // mp3_album
		row = append(row, status)    // status

		writer.Write(row)
		seqCounter++
	}

	// Write Header Row (Seq 0)
	headerRow := []string{"0"}
	headerRow = append(headerRow, headers...)
	headerRow = append(headerRow, "matched_filename", "mp3_title", "mp3_artist", "mp3_album", "status")
	writer.Write(headerRow)
	seqCounter++ // Move to Seq 1 for data rows

	// Tracking maps
	fileMatched := make(map[string]bool)
	fileGroupedBySpeaker := make(map[string]bool)
	catalogMatched := make(map[int]bool)

	// ==========================================
	// PASS 1: STRICT FILENAME MATCHING
	// ==========================================
	fmt.Println("Running Pass 1: Strict matching...")

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		filename := f.Name()
		lowerFilename := strings.ToLower(filename)

		for idx, entry := range catalogEntries {
			if catalogMatched[idx] {
				continue
			}

			lowerTitle := strings.ToLower(entry.Title)
			lowerSpeaker := strings.ToLower(entry.Speaker)

			if strings.Contains(lowerFilename, lowerTitle) && strings.Contains(lowerFilename, lowerSpeaker) {
				mp3Title, mp3Artist, mp3Album := extractMP3Tags(audioDirPath, filename)
				writeOutputRow(entry.OriginalRow, filename, mp3Title, mp3Artist, mp3Album, "Strict Matched")
				fileMatched[filename] = true
				catalogMatched[idx] = true
				break
			}
		}
	}

	// ==========================================
	// PASS 1.5: ID3 TAG MATCHING (NEW)
	// ==========================================
	fmt.Println("Running Pass 1.5: ID3 Tag matching...")

	for _, f := range files {
		if f.IsDir() || fileMatched[f.Name()] {
			continue
		}
		filename := f.Name()

		// Only read tags if it looks like an audio file
		if !strings.HasSuffix(strings.ToLower(filename), ".mp3") &&
			!strings.HasSuffix(strings.ToLower(filename), ".m4a") &&
			!strings.HasSuffix(strings.ToLower(filename), ".flac") {
			continue
		}

		mp3Title, mp3Artist, mp3Album := extractMP3Tags(audioDirPath, filename)
		lowerMp3Title := strings.ToLower(mp3Title)
		lowerMp3Artist := strings.ToLower(mp3Artist)

		// If tags are empty, we can't match on them
		if lowerMp3Title == "" && lowerMp3Artist == "" {
			continue
		}

		for idx, entry := range catalogEntries {
			if catalogMatched[idx] {
				continue
			}

			lowerTitle := strings.ToLower(entry.Title)
			lowerSpeaker := strings.ToLower(entry.Speaker)

			// Compare clean ID3 tags against clean catalog entries
			if (lowerMp3Title != "" && lowerMp3Title == lowerTitle) &&
				(lowerMp3Artist != "" && lowerMp3Artist == lowerSpeaker) {

				writeOutputRow(entry.OriginalRow, filename, mp3Title, mp3Artist, mp3Album, "ID3 Tag Matched")
				fileMatched[filename] = true
				catalogMatched[idx] = true
				break
			}
		}
	}

	// ==========================================
	// PASS 2: FUZZY / NORMALIZED MATCHING
	// ==========================================
	fmt.Println("Running Pass 2: Fuzzy matching (alphanumeric only)...")

	for _, f := range files {
		if f.IsDir() || fileMatched[f.Name()] {
			continue
		}
		filename := f.Name()
		normFilename := normalizeAlphanumeric(filename)

		for idx, entry := range catalogEntries {
			if catalogMatched[idx] {
				continue
			}

			normSpeaker := normalizeAlphanumeric(entry.Speaker)
			normTitle := normalizeAlphanumeric(entry.Title)

			if strings.Contains(normFilename, normSpeaker) && strings.Contains(normFilename, normTitle) {
				mp3Title, mp3Artist, mp3Album := extractMP3Tags(audioDirPath, filename)
				writeOutputRow(entry.OriginalRow, filename, mp3Title, mp3Artist, mp3Album, "Fuzzy Matched")
				fileMatched[filename] = true
				catalogMatched[idx] = true
				break
			}
		}
	}

	// ==========================================
	// PASS 3: SPEAKER GROUPING (Manual Reconciliation)
	// ==========================================
	fmt.Println("Running Pass 3: Grouping by Speaker...")

	remainingSpeakers := make(map[string]bool)
	for idx, entry := range catalogEntries {
		if !catalogMatched[idx] && entry.Speaker != "" {
			remainingSpeakers[entry.Speaker] = true
		}
	}

	for speaker := range remainingSpeakers {
		normSpeaker := normalizeAlphanumeric(speaker)

		for _, f := range files {
			if f.IsDir() || fileMatched[f.Name()] || fileGroupedBySpeaker[f.Name()] {
				continue
			}
			filename := f.Name()
			normFilename := normalizeAlphanumeric(filename)

			if strings.Contains(normFilename, normSpeaker) {
				mp3Title, mp3Artist, mp3Album := extractMP3Tags(audioDirPath, filename)
				// For partial matches, output empty catalog row, but include filename and tags
				writeOutputRow([]string{}, filename, mp3Title, mp3Artist, mp3Album, "Partial: Speaker Group")
				fileGroupedBySpeaker[filename] = true
			}
		}

		for idx, entry := range catalogEntries {
			if catalogMatched[idx] {
				continue
			}
			if entry.Speaker == speaker {
				writeOutputRow(entry.OriginalRow, "", "", "", "", "Partial: Speaker Group")
				catalogMatched[idx] = true
			}
		}
	}

	// ==========================================
	// PASS 4: WRITE REMAINING UNMATCHED
	// ==========================================
	fmt.Println("Writing completely unmatched items...")

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		filename := f.Name()
		if !fileMatched[filename] && !fileGroupedBySpeaker[filename] {
			mp3Title, mp3Artist, mp3Album := extractMP3Tags(audioDirPath, filename)
			writeOutputRow([]string{}, filename, mp3Title, mp3Artist, mp3Album, "File Not In Catalog")
		}
	}

	for idx, entry := range catalogEntries {
		if !catalogMatched[idx] {
			writeOutputRow(entry.OriginalRow, "", "", "", "", "Catalog Not In Files")
		}
	}

	fmt.Printf("Matching complete. Enriched catalog saved to %s\n", outputCSVPath)
}

// readCatalog (Unchanged from previous)
func readCatalog(filePath string) ([]string, map[int]CatalogEntry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, err
	}

	if len(records) == 0 {
		return nil, nil, fmt.Errorf("catalog CSV is empty")
	}

	headers := records[0]
	if len(headers) > 0 {
		headers[0] = strings.TrimPrefix(headers[0], "\ufeff")
	}

	titleIdx := -1
	speakerIdx := -1

	for i, h := range headers {
		cleanHeader := strings.ToLower(strings.TrimSpace(h))
		if cleanHeader == "title" {
			titleIdx = i
		} else if cleanHeader == "speaker" {
			speakerIdx = i
		}
	}

	if titleIdx == -1 || speakerIdx == -1 {
		return nil, nil, fmt.Errorf("catalog CSV must contain 'title' and 'speaker' headers")
	}

	catalog := make(map[int]CatalogEntry)
	for i, row := range records {
		if i == 0 {
			continue
		}
		title := ""
		speaker := ""
		if titleIdx < len(row) {
			title = strings.TrimSpace(row[titleIdx])
		}
		if speakerIdx < len(row) {
			speaker = strings.TrimSpace(row[speakerIdx])
		}

		catalog[i] = CatalogEntry{OriginalRow: row, Title: title, Speaker: speaker}
	}

	return headers, catalog, nil
}
