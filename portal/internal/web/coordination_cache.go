package web

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	coordinationTTL        = 2 * time.Second
	coordinationTimeout    = 5 * time.Second
	coordinationMaxEntries = 128
	coordinationMaxBytes   = 16 * 1024 * 1024
	coordinationMaxFlights = 128
)

var errCoordinationUnavailable = errors.New("Operations cannot be read: ")

type coordinationKey struct {
	workspace, name, slug string
	profile               hostProfileIdentity
	thread, incarnation   string
}

type coordinationSnapshot struct {
	payload []byte
	expires time.Time
	used    uint64
}

type coordinationFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	epoch   uint64
	payload []byte
	err     error
}

// This cache contains display bytes only. Identity and mutation admission stay
// with their existing owners and are checked independently of cache lifetime.
type coordinationCache struct {
	mu      sync.Mutex
	parent  context.Context
	now     func() time.Time
	entries map[coordinationKey]coordinationSnapshot
	flights map[coordinationKey]*coordinationFlight
	bytes   int
	epoch   uint64
	used    uint64
	closed  bool
	wg      sync.WaitGroup
}

func newCoordinationCache(parent context.Context) *coordinationCache {
	return &coordinationCache{parent: parent, now: time.Now,
		entries: make(map[coordinationKey]coordinationSnapshot),
		flights: make(map[coordinationKey]*coordinationFlight)}
}

func (c *coordinationCache) read(ctx context.Context, key coordinationKey,
	load func(context.Context) ([]byte, error), validate func() error,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed || c.parent.Err() != nil {
		c.mu.Unlock()
		return nil, errCoordinationUnavailable
	}
	c.expireLocked(c.now())
	if entry, ok := c.entries[key]; ok {
		c.used++
		entry.used = c.used
		c.entries[key] = entry
		c.mu.Unlock()
		return entry.payload, nil
	}
	flight := c.flights[key]
	if flight == nil {
		if len(c.flights) >= coordinationMaxFlights {
			c.mu.Unlock()
			return nil, errCoordinationUnavailable
		}
		child, cancel := context.WithTimeout(c.parent, coordinationTimeout)
		flight = &coordinationFlight{done: make(chan struct{}), cancel: cancel, epoch: c.epoch}
		c.flights[key] = flight
		c.wg.Add(1)
		go c.refresh(child, key, flight, load, validate)
	}
	flight.waiters++
	c.mu.Unlock()

	defer c.depart(flight)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.parent.Done():
		return nil, errCoordinationUnavailable
	case <-flight.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed || c.parent.Err() != nil || flight.epoch != c.epoch || ctx.Err() != nil {
			return nil, errCoordinationUnavailable
		}
		return flight.payload, flight.err
	}
}

func (c *coordinationCache) refresh(ctx context.Context, key coordinationKey,
	flight *coordinationFlight, load func(context.Context) ([]byte, error), validate func() error,
) {
	defer c.wg.Done()
	defer flight.cancel()
	payload, err := load(ctx)
	if err == nil && !json.Valid(payload) {
		err = errors.New("Operations returned an invalid response.")
	}
	if err == nil {
		err = validate()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || c.closed || flight.epoch != c.epoch {
		err = errCoordinationUnavailable
	}
	if err == nil {
		// The helper's buffer is now immutable and shared with its waiters.
		flight.payload = payload
		c.expireLocked(c.now())
		if len(payload) <= coordinationMaxBytes {
			for len(c.entries) >= coordinationMaxEntries || c.bytes+len(payload) > coordinationMaxBytes {
				c.evictLocked()
			}
			c.used++
			c.entries[key] = coordinationSnapshot{payload: payload,
				expires: c.now().Add(coordinationTTL), used: c.used}
			c.bytes += len(payload)
		}
	}
	flight.err = err
	delete(c.flights, key)
	close(flight.done)
}

func (c *coordinationCache) depart(flight *coordinationFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flight.waiters--
	if flight.waiters == 0 {
		flight.cancel()
	}
}

func (c *coordinationCache) expireLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			c.bytes -= len(entry.payload)
			delete(c.entries, key)
		}
	}
}

func (c *coordinationCache) evictLocked() {
	var oldest coordinationKey
	var used uint64
	for key, entry := range c.entries {
		if used == 0 || entry.used < used {
			oldest, used = key, entry.used
		}
	}
	c.bytes -= len(c.entries[oldest].payload)
	delete(c.entries, oldest)
}

func (c *coordinationCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	clear(c.entries)
	c.bytes = 0
	for _, flight := range c.flights {
		// Retain the flight until its helper has exited. Invalidation must not
		// permit a second helper for a key whose old helper is still running.
		flight.cancel()
	}
}

func (c *coordinationCache) close() {
	c.mu.Lock()
	c.closed = true
	for _, flight := range c.flights {
		flight.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}
