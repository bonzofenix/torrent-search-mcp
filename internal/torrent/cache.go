package torrent

import (
	"sort"
	"sync"
	"time"
)

type cachedTorrent struct {
	torrent   Torrent
	timestamp int64
	seq       uint64 // insertion order, the tie-breaker Python's dict gives for free
}

// Cache is the long-lived torrent cache used only by get_torrent magnet
// lookups.
//
// Search results are never served directly from this cache; they are only
// stored here so that a later get_torrent call can return a magnet link
// without re-running the same query. Entries expire by TTL and by max size.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*cachedTorrent
	ttl     int64
	maxSize int
	seq     uint64
	now     func() int64
}

// NewCache returns a cache with the given TTL and max size (<= 0 = unbounded).
func NewCache(ttl time.Duration, maxSize int) *Cache {
	return &Cache{
		entries: map[string]*cachedTorrent{},
		ttl:     int64(ttl / time.Second),
		maxSize: maxSize,
		now:     func() int64 { return time.Now().Unix() },
	}
}

// Clean drops entries older than the TTL.
func (c *Cache) Clean() {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := c.now() - c.ttl
	for id, entry := range c.entries {
		if entry.timestamp <= deadline {
			delete(c.entries, id)
		}
	}
}

// Update stores torrents under their ids, evicting the oldest past max size.
func (c *Cache) Update(torrents []Torrent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	timestamp := c.now()
	for _, t := range torrents {
		if entry, ok := c.entries[t.ID]; ok {
			entry.torrent, entry.timestamp = t, timestamp
			continue
		}
		c.seq++
		c.entries[t.ID] = &cachedTorrent{torrent: t, timestamp: timestamp, seq: c.seq}
	}
	over := len(c.entries) - c.maxSize
	if c.maxSize <= 0 || over <= 0 {
		return
	}
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := c.entries[ids[i]], c.entries[ids[j]]
		if a.timestamp != b.timestamp {
			return a.timestamp < b.timestamp
		}
		return a.seq < b.seq
	})
	for _, id := range ids[:over] {
		delete(c.entries, id)
	}
}

// Get returns a cached torrent and refreshes its timestamp.
func (c *Cache) Get(id string) (Torrent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return Torrent{}, false
	}
	entry.timestamp = c.now()
	return entry.torrent, true
}

// Len reports the number of cached entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
