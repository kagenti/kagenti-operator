package cache

import (
	"container/list"
	"sync"
	"time"
)

const (
	DefaultBundleMaxEntries = 100
	DefaultBundleTTL        = 1 * time.Minute
)

type bundleEntry struct {
	key     string
	data    []byte
	etag    string
	expires time.Time
}

type BundleCache struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	entries    map[string]*list.Element
	evictList  *list.List
}

func NewBundleCache(maxEntries int, ttl time.Duration) *BundleCache {
	if maxEntries <= 0 {
		maxEntries = DefaultBundleMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultBundleTTL
	}
	return &BundleCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		entries:    make(map[string]*list.Element),
		evictList:  list.New(),
	}
}

func (c *BundleCache) Get(agentID string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[agentID]
	if !ok {
		return nil, "", false
	}
	e := el.Value.(*bundleEntry)
	if time.Now().After(e.expires) {
		c.removeElement(el)
		return nil, "", false
	}
	c.evictList.MoveToFront(el)
	return e.data, e.etag, true
}

func (c *BundleCache) Set(agentID string, data []byte, etag string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[agentID]; ok {
		c.evictList.MoveToFront(el)
		e := el.Value.(*bundleEntry)
		e.data = data
		e.etag = etag
		e.expires = time.Now().Add(c.ttl)
		return
	}

	e := &bundleEntry{
		key:     agentID,
		data:    data,
		etag:    etag,
		expires: time.Now().Add(c.ttl),
	}
	el := c.evictList.PushFront(e)
	c.entries[agentID] = el

	if c.evictList.Len() > c.maxEntries {
		c.removeLRU()
	}
}

func (c *BundleCache) Invalidate(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[agentID]; ok {
		c.removeElement(el)
	}
}

func (c *BundleCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element)
	c.evictList.Init()
}

func (c *BundleCache) InvalidateFunc(fn func(agentID string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, el := range c.entries {
		if fn(id) {
			c.removeElement(el)
			delete(c.entries, id)
		}
	}
}

func (c *BundleCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictList.Len()
}

func (c *BundleCache) removeLRU() {
	el := c.evictList.Back()
	if el != nil {
		c.removeElement(el)
	}
}

func (c *BundleCache) removeElement(el *list.Element) {
	c.evictList.Remove(el)
	e := el.Value.(*bundleEntry)
	delete(c.entries, e.key)
}
