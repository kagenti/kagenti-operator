package cache

import "sync"

type PolicyEntry struct {
	Path    string
	Content string
}

type policyData struct {
	policies []PolicyEntry
	hash     string
}

type PolicyCache struct {
	mu     sync.RWMutex
	global *policyData
	ns     map[string]*policyData
}

func NewPolicyCache() *PolicyCache {
	return &PolicyCache{
		ns: make(map[string]*policyData),
	}
}

func (c *PolicyCache) GetGlobal() ([]PolicyEntry, string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.global == nil {
		return nil, "", false
	}
	return c.global.policies, c.global.hash, true
}

func (c *PolicyCache) SetGlobal(policies []PolicyEntry, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = &policyData{policies: policies, hash: hash}
}

func (c *PolicyCache) InvalidateGlobal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = nil
}

func (c *PolicyCache) GetNamespace(ns string) ([]PolicyEntry, string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.ns[ns]
	if !ok {
		return nil, "", false
	}
	return d.policies, d.hash, true
}

func (c *PolicyCache) SetNamespace(ns string, policies []PolicyEntry, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ns[ns] = &policyData{policies: policies, hash: hash}
}

func (c *PolicyCache) InvalidateNamespace(ns string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ns, ns)
}
