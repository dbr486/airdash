package routes

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrCooldown is returned while a callsign is in its post-error cooldown
// window; the caller should treat the aircraft as route-unknown for this
// tick rather than retrying immediately.
var ErrCooldown = errors.New("routes: lookup in cooldown")

type cacheEntry struct {
	route   Route
	ok      bool
	err     bool
	expires time.Time
}

// Cache remembers resolved routes per callsign so a flight overhead for
// several minutes costs one upstream call. Only the tracker calls it, one
// callsign at a time, so duplicate in-flight lookups are not a concern
// and a plain mutex is enough.
//
// A lookup error is never stored as a resolved (known or unknown) result;
// instead the callsign is placed in a cooldown of errorCooldown during
// which further calls return ErrCooldown without reaching inner, guarding
// a flaky or rate-limited upstream against a retry storm.
type Cache struct {
	inner         Lookup
	knownTTL      time.Duration
	unknownTTL    time.Duration
	errorCooldown time.Duration
	now           func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

func NewCache(inner Lookup, knownTTL, unknownTTL, errorCooldown time.Duration, now func() time.Time) *Cache {
	return &Cache{
		inner:         inner,
		knownTTL:      knownTTL,
		unknownTTL:    unknownTTL,
		errorCooldown: errorCooldown,
		now:           now,
		entries:       map[string]cacheEntry{},
	}
}

func (c *Cache) Route(ctx context.Context, callsign string) (Route, bool, error) {
	now := c.now()
	c.mu.Lock()
	e, hit := c.entries[callsign]
	c.mu.Unlock()
	if hit && now.Before(e.expires) {
		if e.err {
			return Route{}, false, ErrCooldown
		}
		return e.route, e.ok, nil
	}

	r, ok, err := c.inner.Route(ctx, callsign)
	if err != nil {
		c.mu.Lock()
		c.entries[callsign] = cacheEntry{err: true, expires: now.Add(c.errorCooldown)}
		c.mu.Unlock()
		return Route{}, false, err
	}
	ttl := c.unknownTTL
	if ok {
		ttl = c.knownTTL
	}
	c.mu.Lock()
	c.entries[callsign] = cacheEntry{route: r, ok: ok, expires: now.Add(ttl)}
	// Opportunistic sweep so the map does not grow forever on a busy
	// approach path; cheap because entries number in the hundreds at most.
	for k, v := range c.entries {
		if !now.Before(v.expires) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
	return r, ok, nil
}
