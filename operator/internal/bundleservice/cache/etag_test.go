package cache

import (
	"strings"
	"testing"
)

func TestETagCache_SetAndGet(t *testing.T) {
	c := NewETagCache()
	c.Set("agent1", "sha256:abc")

	hash, ok := c.Get("agent1")
	if !ok {
		t.Fatal("expected hit")
	}
	if hash != "sha256:abc" {
		t.Fatalf("unexpected hash: %s", hash)
	}
}

func TestETagCache_Miss(t *testing.T) {
	c := NewETagCache()
	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected miss")
	}
}

func TestETagCache_Invalidate(t *testing.T) {
	c := NewETagCache()
	c.Set("agent1", "hash")
	c.Invalidate("agent1")

	_, ok := c.Get("agent1")
	if ok {
		t.Fatal("expected miss after invalidation")
	}
}

func TestETagCache_InvalidateAll(t *testing.T) {
	c := NewETagCache()
	c.Set("a", "h1")
	c.Set("b", "h2")
	c.Set("c", "h3")
	c.InvalidateAll()

	if c.Len() != 0 {
		t.Fatalf("expected 0 entries, got %d", c.Len())
	}
}

func TestETagCache_InvalidateFunc(t *testing.T) {
	c := NewETagCache()
	c.Set("ns1-agent1", "h1")
	c.Set("ns1-agent2", "h2")
	c.Set("ns2-agent3", "h3")

	c.InvalidateFunc(func(id string) bool {
		return strings.HasPrefix(id, "ns1-")
	})

	if c.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", c.Len())
	}
	_, ok := c.Get("ns2-agent3")
	if !ok {
		t.Fatal("expected ns2-agent3 to survive")
	}
}
