package main

import (
	"sync"
	"time"
)

// flightCache stores ready-to-send JSON per key and guarantees:
//   - at most ONE database query per key is running at any moment
//     (1,000 identical requests -> 1 query, the rest share the answer);
//   - stale-while-revalidate: once a value exists, readers are NEVER blocked;
//     an expired value is returned instantly while one goroutine refreshes it.
type flightCache struct {
	mu      sync.Mutex
	entries map[string]*flightEntry
	max     int
}

type flightEntry struct {
	val     []byte
	at      time.Time
	loading bool
	done    chan struct{} // closed when the first load finishes
}

func newFlightCache(max int) *flightCache {
	return &flightCache{entries: map[string]*flightEntry{}, max: max}
}

func (c *flightCache) get(key string, ttl time.Duration, load func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		if len(c.entries) >= c.max { // bound memory
			c.entries = map[string]*flightEntry{}
		}
		e = &flightEntry{done: make(chan struct{})}
		c.entries[key] = e
	}
	switch {
	case e.val != nil && time.Since(e.at) < ttl: // fresh
		v := e.val
		c.mu.Unlock()
		return v, nil
	case e.val != nil: // stale: serve now, refresh in background (once)
		v := e.val
		if !e.loading {
			e.loading = true
			go c.refresh(key, e, load)
		}
		c.mu.Unlock()
		return v, nil
	case e.loading: // first load already running: wait for it
		done := e.done
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		v := e.val
		c.mu.Unlock()
		if v == nil {
			return nil, errCacheLoad
		}
		return v, nil
	default: // we are the first
		e.loading = true
		c.mu.Unlock()
		c.refresh(key, e, load)
		c.mu.Lock()
		v := e.val
		c.mu.Unlock()
		if v == nil {
			return nil, errCacheLoad
		}
		return v, nil
	}
}

func (c *flightCache) refresh(key string, e *flightEntry, load func() ([]byte, error)) {
	v, err := load()
	c.mu.Lock()
	if err == nil {
		e.val, e.at = v, time.Now()
	}
	first := e.done
	e.loading = false
	c.mu.Unlock()
	select {
	case <-first:
	default:
		close(first)
	}
}

type cacheErr string

func (e cacheErr) Error() string { return string(e) }

const errCacheLoad = cacheErr("data temporarily unavailable")
