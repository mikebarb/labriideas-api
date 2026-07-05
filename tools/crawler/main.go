package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/mikebarb/labriideas-publisher/pkg/storage" // Adjust if your module name is different

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"
)

func init() {
	// Load the .env file from the project root (2 directories up)
	err := godotenv.Load("../../.env")
	if err != nil {
		log.Println("Warning: Could not find .env file, relying on system env vars")
	}
}

func main() {
	// 1. Load R2 Credentials from Environment Variables
	accountID := os.Getenv("R2_ACCOUNT_ID")
	accessKeyID := os.Getenv("R2_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	bucketName := os.Getenv("R2_BUCKET_NAME")

	if accountID == "" || accessKeyID == "" || secretAccessKey == "" || bucketName == "" {
		log.Fatal("Missing required environment variables: R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET_NAME")
	}

	// 2. Configure the AWS SDK to point to Cloudflare R2
	r2Resolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL: fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID),
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		config.WithEndpointResolverWithOptions(r2Resolver),
		config.WithRegion("auto"), // R2 uses "auto" for the region
	)
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	// 3. Create the raw S3 client
	s3Client := s3.NewFromConfig(cfg)

	// 4. Create YOUR storage client using the function you just showed me
	client := storage.NewClient(s3Client, bucketName)

	fmt.Println("Starting R2 Crawl...")

	// 5. Run the Crawl
	catalogData, err := client.CrawlCatalog(context.Background(), nil)
	if err != nil {
		log.Fatalf("Crawl failed: %v", err)
	}

	fmt.Printf("Crawl complete. Found %d tracks.\n", catalogData["count"])

	// 6. Marshal the dynamic map to JSON
	jsonData, err := json.MarshalIndent(catalogData, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal JSON: %v", err)
	}

	// 7. Write the Gzip file to the local disk so we can inspect it
	outputFile := "test-catalog.json.gz"
	outFile, err := os.Create(outputFile)
	if err != nil {
		log.Fatalf("Failed to create file: %v", err)
	}
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	gzWriter.Name = "catalog.json"

	_, err = gzWriter.Write(jsonData)
	if err != nil {
		log.Fatalf("Failed to write gzip data: %v", err)
	}
	gzWriter.Close()

	fmt.Printf("✅ Successfully wrote test catalog to %s\n", outputFile)
}
