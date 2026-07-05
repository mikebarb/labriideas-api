// Package csvio handles reading and writing catalog CSVs in the format
// defined by schema.CatalogSchema. Used by the catalog-exporter,
// csv-reformatter, and any future import/export tools.
package csvio

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/mikebarb/labriideas-publisher/pkg/schema"
)

// ReadCatalogCSV reads a CSV file and returns a map of filename -> metadata.
//
// The first row is treated as the header. Column names are trimmed and
// lowercased for case-insensitive matching against schema.CatalogSchema.
// Extra columns not in the schema are silently ignored.
//
// If the file has no header (first row is data), the function will fail
// with an error because no schema column names are found in the header.
func ReadCatalogCSV(path string) (map[string]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %w", path, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1 // tolerate variable column counts

	// Read the first row — this MUST be the header
	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read CSV header (first row): %w", err)
	}

	// Normalize headers and build a map of column-name -> index
	headerMap := make(map[string]int)
	for i, h := range headers {
		normalized := strings.TrimSpace(strings.ToLower(h))
		headerMap[normalized] = i
	}

	// Ensure the mandatory 'filename' column exists
	if _, ok := headerMap["filename"]; !ok {
		return nil, fmt.Errorf("mandatory 'filename' column missing in CSV header. " +
			"The first row of the file must contain column names, including 'filename'.")
	}

	// Validate at least one schema column is found in the header
	// (otherwise the user might have a headerless CSV)
	schemaColFound := false
	for _, col := range schema.CatalogSchema {
		if _, ok := headerMap[col]; ok {
			schemaColFound = true
			break
		}
	}
	if !schemaColFound {
		return nil, fmt.Errorf("no schema columns found in CSV header. " +
			"The first row appears to be data, not column names. " +
			"Please add a header row (filename, title, artist, ...) to the file.")
	}

	// Read the remaining rows
	catalog := make(map[string]map[string]string)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading CSV data row: %w", err)
		}

		// Extract the filename
		filename := ""
		if idx, ok := headerMap["filename"]; ok && idx < len(row) {
			filename = strings.TrimSpace(row[idx])
		}
		if filename == "" {
			continue // silently skip rows without a filename
		}

		// Check for duplicate filename — warn and keep the last one
		if _, exists := catalog[filename]; exists {
			fmt.Printf("⚠️  Duplicate filename found: %s — keeping the last occurrence.\n", filename)
		}

		// Build metadata using the schema column order
		meta := make(map[string]string)
		for _, col := range schema.CatalogSchema {
			if idx, ok := headerMap[col]; ok && idx < len(row) {
				meta[col] = strings.TrimSpace(row[idx])
			}
		}
		catalog[filename] = meta
	}

	return catalog, nil
}

// WriteCatalogCSV writes the catalog to a CSV file using the column order
// from schema.CatalogSchema. Rows are sorted by filename for a stable,
// diff-friendly output.
//
// The map key is always written as the "filename" column, regardless of
// whether the metadata map contains a "filename" field. This makes the
// function correct for both:
//   - Catalogs where filename is the map key (e.g., fetched from the API)
//   - Catalogs where filename is in the metadata (e.g., read from a CSV)
//
// Returns the number of data rows written (excluding the header).
func WriteCatalogCSV(path string, catalogMap map[string]map[string]string) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("could not create %s: %w", path, err)
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	defer writer.Flush()

	// Write header row
	if err := writer.Write(schema.CatalogSchema); err != nil {
		return 0, fmt.Errorf("failed to write CSV header: %w", err)
	}

	// Sort filenames for stable output
	filenames := make([]string, 0, len(catalogMap))
	for fn := range catalogMap {
		filenames = append(filenames, fn)
	}
	sort.Strings(filenames)

	// Write each data row
	rowCount := 0
	for _, fn := range filenames {
		meta := catalogMap[fn]
		row := make([]string, len(schema.CatalogSchema))
		for i, col := range schema.CatalogSchema {
			// Special case: the map key is always the filename,
			// even if the metadata doesn't contain a "filename" field.
			if col == "filename" {
				row[i] = fn
			} else {
				row[i] = meta[col] // empty string if missing
			}
		}
		if err := writer.Write(row); err != nil {
			return rowCount, fmt.Errorf("failed to write row for %s: %w", fn, err)
		}
		rowCount++
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return rowCount, err
	}

	return rowCount, nil
}

// SortedFilenames returns a sorted slice of filenames from a catalog map.
// Useful for iterating over a catalog in a predictable order.
func SortedFilenames(catalogMap map[string]map[string]string) []string {
	out := make([]string, 0, len(catalogMap))
	for fn := range catalogMap {
		out = append(out, fn)
	}
	sort.Strings(out)
	return out
}
