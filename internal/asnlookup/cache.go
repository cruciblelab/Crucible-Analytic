package asnlookup

import (
	"container/list"
	"net/netip"
	"sync"
	"time"
)

// ttlCache is a fixed-capacity, least-recently-used cache with a fixed
// per-entry TTL, keyed by IP. Hand-rolled rather than a dependency: it's a
// standard container/list + map LRU (as e.g. groupcache's lru.Cache
// demonstrates fits in well under 100 lines), consistent with this
// project's existing preference for hand-rolling simple, well-understood
// data structures over pulling in a library for them (see internal/ja4,
// internal/limiter).
//
// Both get and set are safe for concurrent use, guarded by a single
// mutex - a plain Mutex rather than RWMutex, since get itself mutates
// (move-to-front on hit), so there's no read-only path to give an RWMutex
// an advantage on.
type ttlCache struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	now        func() time.Time // overridable in tests; defaults to time.Now
	ll         *list.List
	items      map[netip.Addr]*list.Element
}

type cacheEntry struct {
	key       netip.Addr
	result    Result
	expiresAt time.Time
}

// newTTLCache creates a cache holding at most maxEntries live results,
// each expiring ttl after it was last set. maxEntries and ttl must both
// be positive - see config validation, which enforces this the same way
// it enforces limiter's throttle_queue_size.
func newTTLCache(maxEntries int, ttl time.Duration) *ttlCache {
	return &ttlCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
		ll:         list.New(),
		items:      make(map[netip.Addr]*list.Element),
	}
}

func (c *ttlCache) get(key netip.Addr) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return Result{}, false
	}
	entry := el.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		c.removeElement(el)
		return Result{}, false
	}
	c.ll.MoveToFront(el)
	return entry.result, true
}

func (c *ttlCache) set(key netip.Addr, result Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	expiresAt := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.result = result
		entry.expiresAt = expiresAt
		c.ll.MoveToFront(el)
		return
	}

	if c.ll.Len() >= c.maxEntries {
		if oldest := c.ll.Back(); oldest != nil {
			c.removeElement(oldest)
		}
	}
	el := c.ll.PushFront(&cacheEntry{key: key, result: result, expiresAt: expiresAt})
	c.items[key] = el
}

// len reports the number of entries currently held, including any that
// are logically expired but not yet evicted (eviction is lazy, on get) -
// exported to tests only via being unexported and same-package.
func (c *ttlCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *ttlCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	entry := el.Value.(*cacheEntry)
	delete(c.items, entry.key)
}
