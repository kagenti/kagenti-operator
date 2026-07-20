package cache

import "sync"

type ETagCache struct {
	mu      sync.RWMutex
	entries map[string]string // clientID → content hash
}

func NewETagCache() *ETagCache {
	return &ETagCache{
		entries: make(map[string]string),
	}
}

func (c *ETagCache) Get(agentID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	hash, ok := c.entries[agentID]
	return hash, ok
}

func (c *ETagCache) Set(agentID, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[agentID] = hash
}

func (c *ETagCache) Invalidate(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, agentID)
}

func (c *ETagCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]string)
}

func (c *ETagCache) InvalidateFunc(fn func(agentID string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.entries {
		if fn(id) {
			delete(c.entries, id)
		}
	}
}

func (c *ETagCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
