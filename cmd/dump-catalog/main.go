package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"

	"github.com/mikebarb/labriideas-publisher/pkg/storage"
)

func main() {
	godotenv.Load()

	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	accountID := os.Getenv("R2_ACCOUNT_ID")
	bucketName := os.Getenv("R2_BUCKET_NAME")

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		log.Fatal(err)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + accountID + ".r2.cloudflarestorage.com")
	})

	client := storage.NewClient(s3Client, bucketName)
	compressed, err := client.GetObjectBytes(context.Background(), "catalog.json.gz")
	if err != nil {
		log.Fatalf("GetObjectBytes failed: %v", err)
	}

	// Write the raw gzipped file
	if err := os.WriteFile("catalog.json.gz", compressed, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote %d bytes to catalog.json.gz\n", len(compressed))

	// Decompress
	gzReader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		log.Fatalf("Failed to decompress: %v", err)
	}
	defer gzReader.Close()

	jsonBytes, err := io.ReadAll(gzReader)
	if err != nil {
		log.Fatalf("Failed to read decompressed bytes: %v", err)
	}

	// Write the plain JSON
	if err := os.WriteFile("catalog.json", jsonBytes, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote %d bytes to catalog.json\n", len(jsonBytes))
}
