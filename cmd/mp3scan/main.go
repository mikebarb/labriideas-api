package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dhowden/tag"
	"github.com/ncruces/zenity"
)

// dotInterval: a progress dot is printed for the first MP3 in each
// directory, then one dot for every dotInterval files after that.
const dotInterval = 10

// header is the fixed schema. Copyright is placed at the end to keep
// all prior column indexes identical. Extra unmapped tags spill out to the right.
var header = []string{
	"file_path",
	"dir_path",
	"file_name",
	"Title",
	"Artist",
	"Album_Artist",
	"Album",
	"Year",
	"Genre",
	"Track_Number",
	"Disc_Number",
	"Composer",
	"Grouping",
	"Comment",
	"Copyright",
}

// handledKeys tracks which raw tags are explicitly mapped to our fixed columns.
// Any tag not in this map will be appended to the row as "tag_name:value" (or "tag_name:skipped").
var handledKeys = map[string]bool{
	"TIT2": true, //Title / Song name (Mapped to our Title column
	"TPE1": true, //Lead performer(s) / Soloist(s) (Mapped to our Artist column)
	"TPE2": true, //Album Artist / Band / Orchestra (Mapped to our Album_Artist column)
	"TALB": true, //Album / Collection (Mapped to our Album column)
	"TYER": true, //Year (Mapped to our Year column)
	"TDRC": true, //Alternate ID3v2.4 Year (Mapped to our Year column)
	"TCON": true, //Content Type / Genre (Mapped to our Genre column)
	"TRCK": true, //Track number / Position in set (Mapped to our Track_Number column)
	"TPOS": true, //Part of a set / Disc number (Mapped to our Disc_Number column)
	"TCOM": true, //Composer (Mapped into your Composer column)
	"TIT1": true, //Content Group Description (Mapped to our Grouping column)
	"COMM": true, //Comments (Mapped to our Comment column)
	"TCOP": true, //Copyright (Mapped to our Copyright column)
}

func main() {
	// Ensure the console window stays open until the user presses Enter.
	defer func() {
		fmt.Print("\nPress Enter to close...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}()

	startTime := time.Now()

	// ---- 1. Chooser: directory to scan ----
	scanRoot, err := zenity.SelectFile(
		zenity.Title("Select the mp3 directory to scan"),
		zenity.Directory(),
	)
	if err != nil {
		// User cancelled the dialog.
		fmt.Println("Cancelled: no directory selected.")
		return
	}

	// ---- 2. Chooser: output file location and name ----
	outPath, err := zenity.SelectFileSave(
		zenity.Title("Save the inventory file as..."),
		zenity.ConfirmOverwrite(),
		zenity.FileFilters{
			zenity.FileFilter{
				Name:     "CSV file (comma delimited)",
				Patterns: []string{"*.csv"},
			},
			zenity.FileFilter{
				Name:     "TXT file (tab delimited)",
				Patterns: []string{"*.txt"},
			},
		},
	)
	if err != nil {
		// User cancelled the dialog.
		fmt.Println("Cancelled: no output file selected.")
		return
	}

	// ---- 3. Determine delimiter from the chosen extension ----
	// .txt -> tab delimited, anything else (default .csv) -> comma.
	ext := strings.ToLower(filepath.Ext(outPath))
	delimiter := ','
	if ext == ".txt" {
		delimiter = '\t'
	} else if ext != ".csv" {
		// Normalise any other extension to .csv so Excel opens it cleanly.
		outPath += ".csv"
	}

	// ---- 4. Create the output file ----
	outFile, err := os.Create(outPath)
	if err != nil {
		fmt.Printf("Error: cannot create output file %s: %v\n", outPath, err)
		return
	}
	defer outFile.Close()

	writer := csv.NewWriter(outFile)
	writer.Comma = delimiter
	defer writer.Flush()

	// Write the fixed header row first.
	if err := writer.Write(header); err != nil {
		fmt.Printf("Error: cannot write header row: %v\n", err)
		return
	}

	// ---- 5. Walk the directory tree ----
	startProcessingTime := time.Now()
	fmt.Printf("Scanning: %s\n", scanRoot)

	dirCount := 0
	fileCount := 0
	errorCount := 0
	dotCount := 0 // files processed since the last dot in the current directory

	err = filepath.WalkDir(scanRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			fmt.Printf("\nError accessing %s: %v\n", path, err)
			errorCount++
			return nil // skip problem entries but keep walking
		}

		if d.IsDir() {
			// Announce each directory (other than the already-announced root)
			// on its own line.
			if path != scanRoot {
				fmt.Printf("\n%s\n", path)
			}
			dirCount++
			dotCount = 0
			return nil
		}

		// Filter strictly for .mp3 files (case-insensitive).
		if !strings.EqualFold(filepath.Ext(path), ".mp3") {
			return nil
		}

		row := scanFile(path)

		if err := writer.Write(row); err != nil {
			fmt.Printf("\nError writing row for %s: %v\n", path, err)
			errorCount++
		}

		fileCount++

		// ---- Progress dots ----
		// Dot on the first file processed in a directory, then one dot
		// every dotInterval files after that.
		dotCount++
		if dotCount == 1 || dotCount%dotInterval == 0 {
			fmt.Print(".")
		}

		return nil
	})

	if err != nil {
		fmt.Printf("\nError during directory walk: %v\n", err)
		errorCount++
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		fmt.Printf("Error flushing output file: %v\n", err)
	}

	// ---- 6. Completion summary ----
	fmt.Printf("\n\nScan complete.\n")
	fmt.Printf("  Directories scanned: 	%d\n", dirCount)
	fmt.Printf("  MP3 files processed: 	%d\n", fileCount)
	fmt.Printf("  Errors encountered:  	%d\n", errorCount)
	fmt.Printf("  Output file:         	%s\n", outPath)
	fmt.Printf("  Total processingtime: %s\n", time.Since(startProcessingTime).Round(time.Millisecond))
	fmt.Printf("  Total runtime:       	%s\n", time.Since(startTime).Round(time.Millisecond))
}

// scanFile extracts base ID3 tags and appends any unmapped raw tags as trailing columns.
func scanFile(path string) []string {
	// Initialize row with capacity for base header fields
	row := make([]string, len(header))
	for i := range row {
		row[i] = ""
	}

	row[0] = path
	row[1] = filepath.Dir(path)
	row[2] = filepath.Base(path)

	// Collect any unmapped/custom tags found in Raw()
	var extraCols []string

	// ---- ID3 tags ----
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if metadata, err := tag.ReadFrom(f); err == nil {
			row[3] = metadata.Title()
			row[4] = metadata.Artist()
			row[5] = metadata.AlbumArtist()
			row[6] = metadata.Album()

			// Year: integer if parseable, otherwise verbatim string as-is.
			// dhowden/tag exposes raw frames under their uppercase ID3
			// frame IDs: TDRC (v2.4) or TYER (v2.3).
			if rawYear := metadata.Year(); rawYear != 0 {
				row[7] = strconv.Itoa(rawYear)
			} else {
				for _, key := range []string{"TDRC", "TYER"} {
					if raw, ok := metadata.Raw()[key]; ok {
						if s, ok := raw.(string); ok && s != "" {
							row[7] = s
							break
						}
					}
				}
			}

			row[8] = metadata.Genre()

			// Track number: raw TRCK frame verbatim if present (may be
			// "07/15" or any string); integer fallback otherwise.
			if raw, ok := metadata.Raw()["TRCK"]; ok {
				if s, ok := raw.(string); ok {
					row[9] = s
				}
			}
			if row[9] == "" {
				if track, total := metadata.Track(); track != 0 {
					if total != 0 {
						row[9] = fmt.Sprintf("%d/%d", track, total)
					} else {
						row[9] = fmt.Sprintf("%d", track)
					}
				}
			}

			// Disc number: raw TPOS frame verbatim if present (may be
			// "1" or a string like "EN NL"); integer fallback otherwise.
			if raw, ok := metadata.Raw()["TPOS"]; ok {
				if s, ok := raw.(string); ok {
					row[10] = s
				}
			}
			if row[10] == "" {
				if disc, total := metadata.Disc(); disc != 0 {
					if total != 0 {
						row[10] = fmt.Sprintf("%d/%d", disc, total)
					} else {
						row[10] = fmt.Sprintf("%d", disc)
					}
				}
			}

			row[11] = metadata.Composer()

			// dhowden/tag maps TIT1 (content group) into its Raw() map.
			if raw, ok := metadata.Raw()["grouping"]; ok {
				if s, ok := raw.(string); ok {
					row[12] = s
				}
			}

			if comment := metadata.Comment(); comment != "" {
				row[13] = comment
			}

			// Copyright (TCOP frame)
			if raw, ok := metadata.Raw()["copyright"]; ok {
				if s, ok := raw.(string); ok {
					row[14] = s
				}
			}

			// Scan raw tags for anything we didn't explicitly map above
			for k, v := range metadata.Raw() {
				if handledKeys[k] {
					continue
				}
				if s, ok := v.(string); ok {
					if s != "" {
						extraCols = append(extraCols, fmt.Sprintf("%s:%s", k, s))
					}
				} else {
					// Non-string tag (e.g., binary album art APIC frame)
					extraCols = append(extraCols, fmt.Sprintf("%s:skipped", k))
				}
			}
		}
	}

	// Append any discovered extra tags to the end of the row (unheaded columns)
	if len(extraCols) > 0 {
		row = append(row, extraCols...)
	}

	return row
}
