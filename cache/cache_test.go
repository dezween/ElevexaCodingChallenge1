package cache

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestCacheFetch checks basic cache hit/miss logic and statistics.
// It ensures that the first fetch is a MISS (data fetched from server),
// and the second fetch is a HIT (data returned from cache).
func TestCacheFetch(t *testing.T) {
	content := "test content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := fmt.Fprint(w, content)
		if err != nil {
			return
		}
	}))
	defer server.Close()

	cache := NewCache(1 * time.Second)

	// First fetch - should miss
	data, err := cache.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != content {
		t.Errorf("got %s, want %s", string(data), content)
	}

	hits, misses, entries := cache.Stats()
	if hits != 0 || misses != 1 || entries != 1 {
		t.Errorf("got stats hits=%d misses=%d entries=%d, want hits=0 misses=1 entries=1", hits, misses, entries)
	}

	// Second fetch - should hit
	data, err = cache.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != content {
		t.Errorf("got %s, want %s", string(data), content)
	}

	hits, misses, entries = cache.Stats()
	if hits != 1 || misses != 1 || entries != 1 {
		t.Errorf("got stats hits=%d misses=%d entries=%d, want hits=1 misses=1 entries=1", hits, misses, entries)
	}
}

// TestCacheConcurrency checks deduplication of concurrent requests to the same URL.
// Only one real HTTP request should be made, all goroutines must get the same result from cache.
func TestCacheConcurrency(t *testing.T) {
	requestCount := 0
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // Simulate slow response
		_, err := fmt.Fprint(w, "content")
		if err != nil {
			return
		}
	}))
	defer server.Close()

	cache := NewCache(1 * time.Second)
	var wg sync.WaitGroup
	concurrent := 10

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.Fetch(context.Background(), server.URL)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	wg.Wait()

	if requestCount != 1 {
		t.Errorf("got %d requests, want 1 (deduplication failed)", requestCount)
	}

	_, _, entries := cache.Stats()
	if entries != 1 {
		t.Errorf("got %d entries, want 1", entries)
	}
}

// TestCacheTTL checks that cache entries expire after their TTL.
// The first fetch is a MISS, the second within TTL is a HIT, after TTL expires is a MISS again.
func TestCacheTTL(t *testing.T) {
	content := "test content"
	requestCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		_, err := fmt.Fprint(w, content)
		if err != nil {
			return
		}
	}))
	defer server.Close()

	cache := NewCache(500 * time.Millisecond)

	// First fetch
	_, err := cache.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Wait for less than TTL
	time.Sleep(250 * time.Millisecond)
	_, err = cache.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requestCount != 1 {
		t.Errorf("got %d requests, want 1 (cache not working)", requestCount)
	}

	// Wait for TTL to expire
	time.Sleep(300 * time.Millisecond)
	_, err = cache.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requestCount != 2 {
		t.Errorf("got %d requests, want 2 (TTL not working)", requestCount)
	}
}

// TestCacheFailedFetch checks that failed HTTP responses are not cached.
// If the server returns an error, the cache must not store the entry.
func TestCacheFailedFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cache := NewCache(1 * time.Second)

	_, err := cache.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Error("expected error, got nil")
	}

	_, _, entries := cache.Stats()
	if entries != 0 {
		t.Errorf("got %d entries, want 0 (failed fetch was cached)", entries)
	}
}

// TestCacheContextCancel checks that Fetch correctly handles context cancellation.
// If the context is cancelled before the request completes, Fetch must return an error.
func TestCacheContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = fmt.Fprint(w, "content")
	}))
	defer server.Close()

	cache := NewCache(1 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := cache.Fetch(ctx, server.URL)
	if err == nil || ctx.Err() == nil {
		t.Error("expected context cancellation error")
	}
}

// TestCacheCustomTTL checks that custom TTL per request works as expected.
// Entry with a short TTL should expire and be refetched after expiration.
func TestCacheCustomTTL(t *testing.T) {
	content := "short ttl"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, content)
	}))
	defer server.Close()

	cache := NewCache(10 * time.Second)
	shortTTL := 100 * time.Millisecond

	_, err := cache.Fetch(context.Background(), server.URL, shortTTL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Waiting for short TTL to expire
	time.Sleep(150 * time.Millisecond)
	_, err = cache.Fetch(context.Background(), server.URL, shortTTL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCacheParallelDifferentURLs checks that parallel requests to different URLs
// do not interfere and each URL is fetched and cached independently.
func TestCacheParallelDifferentURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, r.URL.Path)
	}))
	defer server.Close()

	cache := NewCache(1 * time.Second)
	var wg sync.WaitGroup
	urls := []string{server.URL + "/a", server.URL + "/b", server.URL + "/c"}
	results := make([]string, len(urls))

	for i, url := range urls {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			data, err := cache.Fetch(context.Background(), url)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results[i] = string(data)
		}(i, url)
	}
	wg.Wait()
	for i, url := range urls {
		if results[i] != "/"+string(url[len(url)-1]) {
			t.Errorf("got %s, want %s", results[i], "/"+string(url[len(url)-1]))
		}
	}
}

// TestCacheRetryAfterFailure checks that after a failed fetch (error response),
// a subsequent successful fetch stores the result in cache.
func TestCacheRetryAfterFailure(t *testing.T) {
	var fail = true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	cache := NewCache(1 * time.Second)
	_, err := cache.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Error("expected error on first fetch")
	}
	fail = false
	data, err := cache.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Errorf("unexpected error on retry: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %s, want ok", string(data))
	}
}

// TestCacheEmptyURL checks that Fetch returns an error for an empty URL.
func TestCacheEmptyURL(t *testing.T) {
	cache := NewCache(1 * time.Second)
	_, err := cache.Fetch(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty URL")
	}
}
