package filesearch

import (
	"context"
	"sync"
	"time"
)

// Cache keeps a recent index per workspace so a mention being typed doesn't
// walk the tree on every keystroke. One build at a time per workspace. An
// index is only useful while someone is typing, so workspaces drop out after a
// short idle spell, and the least recently used past a fixed count.
type Cache struct {
	mu    sync.Mutex
	slots map[string]*slot
}

type slot struct {
	lock chan struct{}
	ix   *Index
	used time.Time
}

const (
	cacheSlots = 16
	cacheIdle  = 2 * time.Minute
)

// Get returns key's index if it is younger than ttl, else builds a new one.
// ctx only bounds the wait for another request's build of the same key; build
// itself must not depend on the request, or an aborted keystroke would leave
// half an index behind for the next one.
func (c *Cache) Get(ctx context.Context, key string, ttl time.Duration, build func() *Index) (*Index, error) {
	c.mu.Lock()
	if c.slots == nil {
		c.slots = map[string]*slot{}
	}
	now := time.Now()
	for k, old := range c.slots {
		if now.Sub(old.used) > cacheIdle {
			delete(c.slots, k)
		}
	}
	s := c.slots[key]
	if s == nil {
		if len(c.slots) >= cacheSlots {
			c.evictLocked()
		}
		s = &slot{lock: make(chan struct{}, 1)}
		c.slots[key] = s
	}
	s.used = now
	c.mu.Unlock()

	select {
	case s.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.lock }()
	if s.ix != nil && time.Since(s.ix.Built) < ttl {
		return s.ix, nil
	}
	s.ix = build()
	return s.ix, nil
}

func (c *Cache) evictLocked() {
	var oldest string
	var at time.Time
	for key, s := range c.slots {
		if oldest == "" || s.used.Before(at) {
			oldest, at = key, s.used
		}
	}
	delete(c.slots, oldest)
}
