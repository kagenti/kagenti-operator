package cache

import (
	"testing"
	"time"
)

func TestBundleCache_SetAndGet(t *testing.T) {
	c := NewBundleCache(10, 1*time.Minute)
	c.Set("agent1", []byte("data"), "sha256:abc")

	data, etag, ok := c.Get("agent1")
	if !ok {
		t.Fatal("expected hit")
	}
	if string(data) != "data" {
		t.Fatalf("unexpected data: %s", data)
	}
	if etag != "sha256:abc" {
		t.Fatalf("unexpected etag: %s", etag)
	}
}

func TestBundleCache_Miss(t *testing.T) {
	c := NewBundleCache(10, 1*time.Minute)
	_, _, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected miss")
	}
}

func TestBundleCache_ExpiresAfterTTL(t *testing.T) {
	c := NewBundleCache(10, 50*time.Millisecond)
	c.Set("agent1", []byte("data"), "hash")

	time.Sleep(60 * time.Millisecond)

	_, _, ok := c.Get("agent1")
	if ok {
		t.Fatal("expected miss after TTL expiry")
	}
}

func TestBundleCache_LRUEviction(t *testing.T) {
	c := NewBundleCache(3, 1*time.Minute)
	c.Set("a", []byte("1"), "h1")
	c.Set("b", []byte("2"), "h2")
	c.Set("c", []byte("3"), "h3")
	c.Set("d", []byte("4"), "h4") // evicts "a"

	_, _, ok := c.Get("a")
	if ok {
		t.Fatal("expected 'a' to be evicted")
	}
	_, _, ok = c.Get("d")
	if !ok {
		t.Fatal("expected 'd' to be present")
	}
}

func TestBundleCache_InvalidateAll(t *testing.T) {
	c := NewBundleCache(10, 1*time.Minute)
	c.Set("a", []byte("1"), "h1")
	c.Set("b", []byte("2"), "h2")
	c.InvalidateAll()

	if c.Len() != 0 {
		t.Fatalf("expected 0 entries, got %d", c.Len())
	}
}

func TestBundleCache_Invalidate(t *testing.T) {
	c := NewBundleCache(10, 1*time.Minute)
	c.Set("a", []byte("1"), "h1")
	c.Invalidate("a")

	_, _, ok := c.Get("a")
	if ok {
		t.Fatal("expected miss after invalidation")
	}
}
