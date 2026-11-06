package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/ncruces/zenity"
)

// passStrips defines the extra character removals for each matching pass,
// cumulative on top of the base normalization. Pass 1 strips nothing
// extra; pass 2 strips "_" and ";"; pass 3 strips "__" and "'".
var passStrips = [][]string{
	{},                         // pass 1: base normalization only
	{"_", ";"},                 // pass 2
	{"__", "_", ";", "'", "-"}, // pass 3
	{"__", "_", ";", "'", "-"}, // pass 4: pass 3 strips + whole-word "part" removal
}

// normalizeKey trims surrounding whitespace, removes a trailing .mp3,
// removes trailing non-letters, applies the pass-specific strips, and
// lower-cases for case-insensitive comparison.
func normalizeKey(s string, pass int) string {
	s = strings.TrimSpace(s)

	// Remove a trailing .mp3 extension, ignoring case.
	if strings.HasSuffix(strings.ToLower(s), ".mp3") {
		s = s[:len(s)-len(".mp3")]
	}

	// Remove trailing characters until the final character is a letter.
	s = strings.TrimRightFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r)
	})

	// Apply this pass's character removals.
	for _, old := range passStrips[pass-1] {
		s = strings.ReplaceAll(s, old, "")
	}

	// Pass 4 only: remove the word "part" (whole word, any case) and
	// any number immediately following it, e.g. "part 1", "Part 12".
	if pass == 4 {
		s = removePartWords(s)
	}

	// Collapse runs of whitespace to a single space, so differing
	// punctuation spacing (e.g. " - " vs " ") can't block a match.
	s = strings.Join(strings.Fields(s), " ")

	// Keep matching case-insensitive.
	return strings.ToLower(s)
}

// removePartWords removes whole-word occurrences of "part" (case-insensitive),
// keeping any following number. Avoids substring hits like "parting".
func removePartWords(s string) string {
	words := strings.Fields(s) // splits on whitespace, drops empties
	out := make([]string, 0, len(words))
	for _, w := range words {
		if !strings.EqualFold(w, "part") {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// readCSV reads a CSV file, returning its header row and data rows.
// readCSV reads a CSV file, returning its header row and data rows.
func readCSV(path string) (headers []string, rows [][]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	// Allow ragged rows; we pad/truncate when writing output.
	r.FieldsPerRecord = -1

	records, err := r.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, fmt.Errorf("file is empty: %s", path)
	}

	// Strip a UTF-8 BOM (EF BB BF) if present at the start of the file.
	// Encoding/csv does not remove it, so the first header would otherwise
	// read as "\ufefffilename" and fail key detection.
	if len(records[0]) > 0 {
		records[0][0] = strings.TrimPrefix(records[0][0], "\ufeff")
	}

	return records[0], records[1:], nil
}

// findKeyColumn locates the key column, accepting either "filename" or
// "file_name" (case-insensitive, whitespace-trimmed).
func findKeyColumn(headers []string) int {
	for i, h := range headers {
		switch normalizeKey(h, 1) {
		case "filename", "file_name":
			return i
		}
	}
	return -1
}

// keyAt safely extracts the normalized key from a row for the given pass.
func keyAt(row []string, idx, pass int) string {
	if idx < len(row) {
		return normalizeKey(row[idx], pass)
	}
	return ""
}

// fitBlock pads with empty strings (or truncates) so a row's data always
// aligns with its file's column block in the output.
func fitBlock(row []string, width int) []string {
	out := make([]string, width)
	copy(out, row)
	return out
}

// prefixedHeaders returns headers each prefixed, e.g. "file1_filename".
func prefixedHeaders(headers []string, prefix string) []string {
	out := make([]string, len(headers))
	for i, h := range headers {
		out[i] = prefix + h
	}
	return out
}

func main() {
	// Ensure the console window stays open until the user presses Enter.
	defer func() {
		fmt.Print("\nPress Enter to close...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}()

	// ---- 1. Chooser: first input file ----
	path1, err := zenity.SelectFile(
		zenity.Title("Select the first CSV file"),
		zenity.FileFilters{{Name: "CSV files", Patterns: []string{"*.csv"}}},
	)
	if err != nil {
		// User cancelled the dialog.
		fmt.Println("Cancelled: no first file selected.")
		return
	}

	// ---- 1. Chooser: second input file ----
	path2, err := zenity.SelectFile(
		zenity.Title("Select the second CSV file"),
		zenity.FileFilters{{Name: "CSV files", Patterns: []string{"*.csv"}}},
	)
	if err != nil {
		// User cancelled the dialog.
		fmt.Println("Cancelled: no second file selected.")
		return
	}

	// ---- 2. Read in the two files ----
	headers1, rows1, err := readCSV(path1)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", path1, err)
		return
	}
	headers2, rows2, err := readCSV(path2)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", path2, err)
		return
	}

	// Either file may use either header name; order of selection is irrelevant.
	keyIdx1 := findKeyColumn(headers1)
	if keyIdx1 == -1 {
		fmt.Printf("Error: %s has no 'filename' or 'file_name' column. Headers: %v\n", path1, headers1)
		return
	}
	keyIdx2 := findKeyColumn(headers2)
	if keyIdx2 == -1 {
		fmt.Printf("Error: %s has no 'filename' or 'file_name' column. Headers: %v\n", path2, headers2)
		return
	}

	// ---- 3. Multi-pass matching ----
	const numPasses = 4

	matched1 := make([]bool, len(rows1))
	matched2 := make([]bool, len(rows2))

	type pair struct {
		i, j, pass int
	}
	var pairs []pair

	for pass := 1; pass <= numPasses; pass++ {
		// Index file 2's not-yet-matched rows by this pass's key.
		index2 := make(map[string][]int)
		for j, row := range rows2 {
			if matched2[j] {
				continue
			}
			k := keyAt(row, keyIdx2, pass)
			if k != "" {
				index2[k] = append(index2[k], j)
			}
		}

		// Walk file 1's not-yet-matched rows in original order.
		for i, row := range rows1 {
			if matched1[i] {
				continue
			}
			k := keyAt(row, keyIdx1, pass)
			if k == "" {
				continue
			}
			for _, j := range index2[k] {
				matched1[i] = true
				matched2[j] = true
				pairs = append(pairs, pair{i, j, pass})
			}
		}
	}

	width1 := len(headers1)
	width2 := len(headers2)

	// ---- 4. Build the output rows ----
	// Header: match column, then file 1's columns, then file 2's columns.
	outHeader := append([]string{"match"},
		append(prefixedHeaders(headers1, "file1_"), prefixedHeaders(headers2, "file2_")...)...)

	output := [][]string{outHeader}

	// Matched pairs, in pass order then file 1's original order.
	for _, p := range pairs {
		outRow := append([]string{fmt.Sprintf("match%d", p.pass)},
			append(fitBlock(rows1[p.i], width1), fitBlock(rows2[p.j], width2)...)...)
		output = append(output, outRow)
	}

	// File 1's unmatched rows, aligned in file 1's column block.
	for i, row := range rows1 {
		if !matched1[i] {
			outRow := append([]string{""},
				append(fitBlock(row, width1), fitBlock(nil, width2)...)...)
			output = append(output, outRow)
		}
	}

	// File 2's unmatched rows, aligned in file 2's column block.
	for j, row := range rows2 {
		if !matched2[j] {
			outRow := append([]string{""},
				append(fitBlock(nil, width1), fitBlock(row, width2)...)...)
			output = append(output, outRow)
		}
	}

	// ---- 5. Chooser: output file ----
	outPath, err := zenity.SelectFileSave(
		zenity.Title("Select where to save the comparison file"),
		zenity.Filename("comparison.csv"),
		zenity.ConfirmOverwrite(),
	)
	if err != nil {
		// User cancelled the dialog.
		fmt.Println("Cancelled: no output file selected.")
		return
	}

	outFile, err := os.Create(outPath)
	if err != nil {
		fmt.Printf("Error creating %s: %v\n", outPath, err)
		return
	}
	defer outFile.Close()

	w := csv.NewWriter(outFile)
	if err := w.WriteAll(output); err != nil {
		fmt.Printf("Error writing %s: %v\n", outPath, err)
		return
	}
	w.Flush()

	// ---- Summary ----
	perPass := make(map[int]int)
	for _, p := range pairs {
		perPass[p.pass]++
	}
	fmt.Printf("Done: %d rows written to %s\n", len(output)-1, outPath)
	for pass := 1; pass <= numPasses; pass++ {
		fmt.Printf("Matched in pass %d: %d\n", pass, perPass[pass])
	}
	fmt.Printf("Unmatched in file 1: %d\n", countUnmatched(matched1))
	fmt.Printf("Unmatched in file 2: %d\n", countUnmatched(matched2))
}

// countUnmatched counts file 2 rows that never matched.
func countUnmatched(matched []bool) int {
	n := 0
	for _, m := range matched {
		if !m {
			n++
		}
	}
	return n
}
