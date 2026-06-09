package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Client is our core wrapper around the S3/R2 SDK
type Client struct {
	s3     *s3.Client
	bucket string
}

// NewClient creates a new storage client.
// It takes an already-configured S3 client and the bucket name.
func NewClient(s3Client *s3.Client, bucketName string) *Client {
	return &Client{
		s3:     s3Client,
		bucket: bucketName,
	}
}

// Helper method to get the bucket name internally
func (c *Client) Bucket() string {
	return c.bucket
}

// GetDownloadURL generates a time-limited presigned URL for a file.
func (c *Client) GetDownloadURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presigner := s3.NewPresignClient(c.s3)

	presignedReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expires))

	if err != nil {
		return "", err
	}

	return presignedReq.URL, nil
}

// GetUploadURL generates a time-limited presigned URL for uploading a file.
func (c *Client) GetUploadURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presigner := s3.NewPresignClient(c.s3)

	presignedReq, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expires))

	if err != nil {
		return "", err
	}

	return presignedReq.URL, nil
}

// GetMetadata fetches only the headers/metadata for a file (Used for ETag checking)
func (c *Client) GetMetadata(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	return c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
}

// GetObjectBytes downloads the entire file into a byte slice (Used for caching the catalog)
func (c *Client) GetObjectBytes(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// UpdateMetadata replaces the custom metadata for an existing object in R2
// R2 does not allow patching metadata directly; we must copy the object over itself
func (c *Client) UpdateMetadata(ctx context.Context, filename string, metadata map[string]string) error {
	// URL-encode the filename! Spaces must become %20 for the S3 CopySource header
	encodedFilename := url.PathEscape(filename)

	// FIX: The CopySource MUST have a leading slash!
	// Format must be: "/bucket-name/encoded-file-name"
	copySource := fmt.Sprintf("/%s/%s", c.bucket, encodedFilename)

	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(c.bucket),
		Key:               aws.String(filename),   // The destination key stays normal (un-encoded)
		CopySource:        aws.String(copySource), // The source must be URL-encoded
		Metadata:          metadata,               // AWS SDK v2 expects standard map[string]string
		MetadataDirective: types.MetadataDirectiveReplace,
	})

	return err
}

// PutObjectBytes uploads a byte slice to R2 at the specified key
func (c *Client) PutObjectBytes(ctx context.Context, key string, data []byte) error {
	reader := bytes.NewReader(data)

	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   reader,
	})

	return err
}

// PutObjectWithMetadata uploads a byte slice to R2 and attaches custom metadata
func (c *Client) PutObjectWithMetadata(ctx context.Context, key string, data []byte, metadata map[string]string) error {
	reader := bytes.NewReader(data)

	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Body:     reader,
		Metadata: metadata, // AWS SDK v2 expects standard map[string]string
	})

	return err
}
