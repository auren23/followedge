package collector

import (
	"sync"
	"time"
)

// Dedup is a short-TTL in-memory set of recently seen event IDs. It exists
// for performance (skip DB writes for overlap between polls); the DB UNIQUE
// constraint on event_id remains the final correctness gate.
type Dedup struct {
	mu    sync.Mutex
	items map[string]time.Time
	ttl   time.Duration
	cap   int
}

func NewDedup(ttl time.Duration, cap int) *Dedup {
	return &Dedup{items: make(map[string]time.Time, 1024), ttl: ttl, cap: cap}
}

// Seen reports whether id was seen within the TTL; inserts it otherwise.
func (d *Dedup) Seen(id string) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.items[id]; ok {
		if now.Sub(t) <= d.ttl {
			return true
		}
		delete(d.items, id) // stale: treat as unseen
	}
	d.items[id] = now
	if len(d.items) > d.cap {
		for k, t := range d.items {
			if now.Sub(t) > d.ttl {
				delete(d.items, k)
			}
		}
	}
	return false
}
