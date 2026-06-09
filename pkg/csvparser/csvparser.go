package csvparser

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/mikebarb/labriideas-publisher/pkg/schema" // Replace with your actual Go module path
)

// ParsedRecord represents a single row from the CSV mapped to our schema
type ParsedRecord struct {
	Filename string
	Metadata map[string]string
}

// Parse reads a CSV and extracts only the fields that match our CatalogSchema
func Parse(file io.Reader) ([]ParsedRecord, error) {
	reader := csv.NewReader(file)

	// 1. Read the header row
	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read CSV headers: %w", err)
	}

	// 2. Build the Header Map (Normalize to lowercase and trim spaces)
	headerMap := make(map[string]int)
	for i, h := range headers {
		normalized := strings.TrimSpace(strings.ToLower(h))
		headerMap[normalized] = i
	}

	// 3. Verify mandatory 'filename' column exists
	if _, exists := headerMap["filename"]; !exists {
		return nil, fmt.Errorf("mandatory 'filename' column missing in CSV")
	}

	var records []ParsedRecord

	// 4. Read the rest of the rows
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading CSV row: %w", err)
		}

		// 5. Extract data based on Schema
		metadata := make(map[string]string)
		filename := ""

		for _, schemaField := range schema.CatalogSchema {
			// Normalize schema field to match our header normalization
			normalizedField := strings.ToLower(schemaField)

			if idx, exists := headerMap[normalizedField]; exists {
				if idx < len(row) {
					value := strings.TrimSpace(row[idx])

					if normalizedField == "filename" {
						filename = value
					} else {
						// Only add to metadata if the value isn't empty
						if value != "" {
							metadata[schemaField] = value // Use original casing from schema
						}
					}
				}
			}
		}

		// Only add if we found a valid filename
		if filename != "" {
			records = append(records, ParsedRecord{
				Filename: filename,
				Metadata: metadata,
			})
		}
	}

	return records, nil
}
