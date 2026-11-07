package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const requestConfigTTL = 30 * time.Second

var errRequestConfigChanged = errors.New("request configuration changed during lookup; retry")

type requestConfigEntry[V any] struct {
	value   V
	expires time.Time
	cost    int
}

// Values are immutable and owned by the cache. Callers must copy mutable output.
// A generation fences both publication and return of an invalidated load. Errors
// are never cached; cancellation does not keep followers waiting for the leader.
type requestConfigCache[K comparable, V any] struct {
	mu       sync.Mutex
	entries  map[K]requestConfigEntry[V]
	versions map[K]uint64
	epoch    uint64
	bytes    int
	flight   singleflight.Group
}

func (c *requestConfigCache[K, V]) invalidate(matches func(K) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if matches == nil {
		c.epoch++
		c.entries = nil
		c.versions = nil
		c.bytes = 0
		return
	}
	for key := range c.versions {
		if matches(key) {
			c.versions[key]++
		}
	}
	for key, entry := range c.entries {
		if matches(key) {
			delete(c.entries, key)
			c.bytes -= entry.cost
		}
	}
}

func (c *requestConfigCache[K, V]) get(ctx context.Context, key K, load func(context.Context) (V, int, error)) (V, error) {
	var zero V
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		c.mu.Lock()
		entry, found := c.entries[key]
		if found && time.Now().Before(entry.expires) {
			c.mu.Unlock()
			return entry.value, nil
		}
		// Keep per-key fences bounded even for failed or oversize loads. A full
		// reset changes the epoch, so no old loader can reuse a reset version.
		if _, known := c.versions[key]; !known && len(c.versions) >= 4096 {
			c.epoch++
			c.entries, c.versions, c.bytes = nil, nil, 0
		}
		if c.versions == nil {
			c.versions = make(map[K]uint64)
		}
		epoch, version := c.epoch, c.versions[key]
		c.versions[key] = version
		c.mu.Unlock()
		result := c.flight.DoChan(fmt.Sprintf("%d:%d:%v", epoch, version, key), func() (any, error) {
			c.mu.Lock()
			if c.epoch != epoch || c.versions[key] != version {
				c.mu.Unlock()
				return nil, errRequestConfigChanged
			}
			if entry, ok := c.entries[key]; ok && time.Now().Before(entry.expires) {
				c.mu.Unlock()
				return entry.value, nil
			}
			c.mu.Unlock()
			value, cost, err := load(ctx)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.epoch != epoch || c.versions[key] != version {
				return nil, errRequestConfigChanged
			}
			// Bound both object count and estimated retained bytes. Large
			// configurations still work, but are deliberately not retained.
			if cost > 0 && cost <= 8<<20 {
				if c.entries == nil {
					c.entries = make(map[K]requestConfigEntry[V])
				}
				now := time.Now()
				for k, e := range c.entries {
					if k == key || !now.Before(e.expires) {
						delete(c.entries, k)
						c.bytes -= e.cost
					}
				}
				for len(c.entries) >= 2048 || c.bytes+cost > 16<<20 {
					for k, e := range c.entries {
						delete(c.entries, k)
						c.bytes -= e.cost
						break
					}
				}
				c.entries[key] = requestConfigEntry[V]{value, now.Add(requestConfigTTL), cost}
				c.bytes += cost
			}
			return value, nil
		})
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case r := <-result:
			if err := ctx.Err(); err != nil {
				return zero, err
			}
			c.mu.Lock()
			unchanged := c.epoch == epoch && c.versions[key] == version
			c.mu.Unlock()
			if !unchanged || errors.Is(r.Err, errRequestConfigChanged) {
				continue
			}
			if r.Err != nil {
				return zero, r.Err
			}
			return r.Val.(V), nil
		}
	}
	return zero, errRequestConfigChanged
}
