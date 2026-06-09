package storage

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mikebarb/labriideas-publisher/pkg/schema"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CrawlProgress holds the data sent back to the server during a crawl
type CrawlProgress struct {
	Percent int    `json:"percent"`
	Message string `json:"message"`
}

// sendProgress is a helper to safely send progress updates without blocking or panicking
func (c *Client) sendProgress(progressChan chan<- CrawlProgress, percent int, message string) {
	if progressChan != nil {
		// Use a select statement to prevent blocking if the channel isn't being read fast enough
		select {
		case progressChan <- CrawlProgress{Percent: percent, Message: message}:
		default:
			// Channel full, skip this update to keep the crawler moving fast
		}
	}
}

// CrawlCatalog scans the entire R2 bucket, reads metadata, and builds a dynamic Catalog map
// It now accepts an optional progressChan. Pass nil to ignore progress (keeps desktop tool working!).
func (c *Client) CrawlCatalog(ctx context.Context, progressChan chan<- CrawlProgress) (map[string]interface{}, error) {

	c.sendProgress(progressChan, 10, "Fetching file list from R2...")

	// 1. Use a slice of dynamic maps instead of a rigid struct
	var tracks []map[string]string
	itemsProcessed := 0

	// 2. Initialize the Paginator
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
	})

	// 3. Loop through pages of objects
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		// 4. Loop through items on this specific page
		for _, obj := range page.Contents {
			key := *obj.Key

			// A. Skip folders
			if strings.HasSuffix(key, "/") {
				continue
			}

			// B. Skip the manifest files
			if key == "catalog.json" || key == "catalog.json.gz" || key == "version.json" {
				continue
			}

			// C. Whitelist: ONLY process MP3 files
			if !strings.HasSuffix(strings.ToLower(key), ".mp3") {
				continue
			}

			// 5. Head the object to get its custom metadata
			headOutput, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(c.bucket),
				Key:    &key,
			})

			if err != nil {
				log.Printf("Warning: Failed to head object %s: %v", key, err)
				continue
			}

			// 6. DYNAMIC MAPPING using the Global Schema
			trackData := make(map[string]string)

			for _, fieldName := range schema.CatalogSchema {
				switch fieldName {
				case "filename":
					// ID and Filename are derived from the R2 Key
					trackData[fieldName] = key
				case "hash":
					// Hash is derived from the R2 ETag (stripping the quotes AWS adds)
					trackData[fieldName] = strings.Trim(*headOutput.ETag, `"`)
				default:
					// For everything else (title, artist, future fields), grab from custom metadata
					// R2 normalizes metadata keys to lowercase, which perfectly matches our lowercase schema
					trackData[fieldName] = headOutput.Metadata[fieldName]
				}
			}

			tracks = append(tracks, trackData)
			itemsProcessed++

			// Update progress every 50 items to avoid flooding the channel
			if itemsProcessed%50 == 0 {
				c.sendProgress(progressChan, 30, fmt.Sprintf("Processed %d files...", itemsProcessed))
			}
		}
	}

	c.sendProgress(progressChan, 90, fmt.Sprintf("Finished processing %d files. Building catalog...", itemsProcessed))

	// 7. Build the final dynamic Catalog map
	catalog := map[string]interface{}{
		"release": fmt.Sprintf("r2-crawl-%s", time.Now().Format("2006-01-02-15-04-05")),
		"count":   len(tracks),
		"tracks":  tracks,
	}

	c.sendProgress(progressChan, 100, "Crawl complete.")

	return catalog, nil
}
