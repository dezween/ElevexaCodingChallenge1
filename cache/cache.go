// Package cache provides a thread-safe HTTP content caching solution with TTL support.
// It implements deduplication of concurrent requests and tracks cache statistics.
// The package is designed to work with standard Go libraries only and provides
// automatic expiration of cached entries based on TTL.
package cache

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// cacheEntry represents a single cache entry with its metadata
type cacheEntry struct {
	data      []byte
	createdAt time.Time
	ttl       time.Duration
}

// pendingRequest represents an ongoing request
type pendingRequest struct {
	done chan struct{}
	data []byte
	err  error
}

// Cache provides thread-safe HTTP content caching with TTL support.
// It ensures that concurrent requests for the same URL are deduplicated
// and maintains statistics about cache hits and misses.
type Cache struct {
	mu         sync.RWMutex
	data       map[string]*cacheEntry
	pending    map[string]*pendingRequest
	defaultTTL time.Duration
	hits       int64
	misses     int64
	client     *http.Client
}

// NewCache creates a new cache instance with the specified default TTL.
// The defaultTTL parameter determines how long entries will be kept in cache
// before being considered stale.
func NewCache(defaultTTL time.Duration) *Cache {
	return &Cache{
		data:       make(map[string]*cacheEntry),
		pending:    make(map[string]*pendingRequest),
		defaultTTL: defaultTTL,
		client:     &http.Client{},
	}
}

// Fetch retrieves content from the cache or fetches it from the URL.
// If the content is present in cache and not expired, it is returned immediately.
// For concurrent requests to the same URL, only one HTTP request is made while
// other goroutines wait for the result.
//
// Parameters:
//   - ctx: Context for request cancellation
//   - url: The URL to fetch content from
//   - ttlOverride: Optional TTL duration that overrides the default TTL for this entry
//
// Returns:
//   - []byte: The fetched content
//   - error: Any error that occurred during fetching or nil on success
func (c *Cache) Fetch(ctx context.Context, url string, ttlOverride ...time.Duration) ([]byte, error) {
	ttl := c.defaultTTL
	if len(ttlOverride) > 0 {
		ttl = ttlOverride[0]
	}

	// Check cache first
	if data, ok := c.getFromCache(url); ok {
		return data, nil
	}

	c.mu.Lock()
	if _, exists := c.pending[url]; exists {
		c.mu.Unlock()
		data, err, _ := c.waitPendingRequest(url)
		return data, err
	}
	// Create new pending request
	pendingReq := &pendingRequest{
		done: make(chan struct{}),
	}
	c.pending[url] = pendingReq
	c.mu.Unlock()

	// Perform the actual fetch
	data, err := c.fetchURL(ctx, url)

	// Store result and notify waiters
	pendingReq.data = data
	pendingReq.err = err
	close(pendingReq.done)

	c.mu.Lock()
	delete(c.pending, url)
	if err == nil {
		c.data[url] = &cacheEntry{
			data:      data,
			createdAt: time.Now(),
			ttl:       ttl,
		}
	}
	c.mu.Unlock()

	return data, err
}

// getFromCache attempts to retrieve and validate a cache entry
func (c *Cache) getFromCache(url string) ([]byte, bool) {
	c.mu.RLock()
	entry, exists := c.data[url]
	c.mu.RUnlock()

	if !exists {
		atomic.AddInt64(&c.misses, 1)
		log.Printf("Cache MISS for URL: %s", url)
		return nil, false
	}

	if time.Since(entry.createdAt) > entry.ttl {
		c.mu.Lock()
		delete(c.data, url)
		c.mu.Unlock()
		atomic.AddInt64(&c.misses, 1)
		log.Printf("Cache MISS (expired) for URL: %s", url)
		return nil, false
	}

	atomic.AddInt64(&c.hits, 1)
	log.Printf("Cache HIT for URL: %s", url)
	return entry.data, true
}

// waitPendingRequest waits for an ongoing request to complete if one exists
func (c *Cache) waitPendingRequest(url string) ([]byte, error, bool) {
	c.mu.RLock()
	req, exists := c.pending[url]
	c.mu.RUnlock()

	if !exists {
		return nil, nil, false
	}

	<-req.done
	return req.data, req.err, true
}

// fetchURL performs the actual HTTP request
func (c *Cache) fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error fetching URL: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("error closing response body: %v", err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	return data, nil
}

// Stats returns cache statistics including hits, misses and current number of entries.
// This method is thread-safe and can be called concurrently with other operations.
//
// Returns:
//   - hits: Number of cache hits
//   - misses: Number of cache misses
//   - entries: Current number of entries in cache
//
// Uses atomic for counters and lock only for map size.
func (c *Cache) Stats() (int, int, int) {
	hits := int(atomic.LoadInt64(&c.hits))
	misses := int(atomic.LoadInt64(&c.misses))
	c.mu.RLock()
	entries := len(c.data)
	c.mu.RUnlock()
	return hits, misses, entries
}
