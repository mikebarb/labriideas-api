package storage

import (
	"context"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// UploadFile uploads a file to R2 with custom metadata.
// fileData can be a local file (os.Open) or an HTTP request body.
func (c *Client) UploadFile(ctx context.Context, key string, fileSize int64, fileData io.Reader, metadata map[string]string) error {

	// 1. Sanitize metadata keys (Force lowercase to prevent R2 sync issues)
	sanitizedMeta := make(map[string]string)
	for k, v := range metadata {
		sanitizedMeta[strings.ToLower(k)] = v
	}

	// 2. Perform the Upload
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Body:     fileData,
		Metadata: sanitizedMeta,
		// ContentLength is highly recommended in Go to prevent memory issues on large files
		ContentLength: aws.Int64(fileSize),
	})

	return err
}
