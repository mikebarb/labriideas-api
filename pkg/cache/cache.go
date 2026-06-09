package cache

import (
	"sync"
	"time"
)

// CatalogCache holds the compressed catalog data and its versioning metadata
type CatalogCache struct {
	mu          sync.RWMutex
	etag        string
	bytes       []byte
	lastChecked time.Time // Tracks when we last verified this ETag with R2 (for TTL)
}

// Update updates the cache with new data and resets the freshness timer
func (c *CatalogCache) Update(etag string, bytes []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.etag = etag
	c.bytes = bytes
	c.lastChecked = time.Now() // Mark as freshly checked
}

// Get returns the cached bytes and etag
func (c *CatalogCache) Get() (string, []byte) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.etag, c.bytes
}

// IsMetadataFresh checks if we've verified the ETag with R2 recently
// ttl is the duration (e.g., 60 seconds) we trust the cache without checking R2
func (c *CatalogCache) IsMetadataFresh(ttl time.Duration) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// If we haven't checked yet, it's not fresh
	if c.lastChecked.IsZero() {
		return "", false
	}

	// If the time since last check is less than the TTL, the ETag is fresh!
	if time.Since(c.lastChecked) < ttl {
		return c.etag, true
	}

	// Otherwise, it's stale and we need to check R2 again
	return "", false
}

// Clear wipes the cache, forcing a fresh download from R2 on the next request
// This is called when an admin updates metadata
func (c *CatalogCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.etag = ""
	c.bytes = nil
	c.lastChecked = time.Time{} // Reset the TTL timer too
}

// NewCatalogCache creates and returns a new, initialized CatalogCache instance.
func NewCatalogCache() *CatalogCache {
	return &CatalogCache{}
}
