package main

// ===================================================
// Generate a token for use with the publisher server.
// This is a one-time operation, and the generated token
// should be copied into the .env file as the value of
// ADMIN_API_TOKENS and PUBLISHER_API_TOKEN.
// ===================================================
import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func main() {
	b := make([]byte, 32)
	rand.Read(b)
	fmt.Println(base64.RawURLEncoding.EncodeToString(b))
}
