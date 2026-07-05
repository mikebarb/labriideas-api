package client

import (
	"os"
	"time"
)

// getenv reads an env var via os.Getenv. Indirected for testability.
func getenv(key string) string {
	return os.Getenv(key)
}

// timeSleep sleeps for d seconds. Indirected for testability.
func timeSleep(d int) {
	time.Sleep(time.Duration(d) * time.Second)
}
