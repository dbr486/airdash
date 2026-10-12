package flightstatus

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCooldown = errors.New("flightstatus: lookup in cooldown")

type entry struct {
	st      Status
	ok      bool
	err     bool
	expires time.Time
}

// Cache remembers a status per callsign so the aircraft overhead for ten
// minutes costs one AeroAPI result set. Errors (including a refused
// budget) enter a short cooldown rather than being retried every tick.
type Cache struct {
	inner         Lookup
	knownTTL      time.Duration
	unknownTTL    time.Duration
	errorCooldown time.Duration
	// authCooldown applies instead of errorCooldown to an ErrAuth failure:
	// a bad key cannot fix itself on the next tick, so retrying it on the
	// short cooldown only burns the daily budget on the same failure.
	authCooldown time.Duration
	now          func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

func NewCache(inner Lookup, knownTTL, unknownTTL, errorCooldown time.Duration, now func() time.Time) *Cache {
	return &Cache{inner: inner, knownTTL: knownTTL, unknownTTL: unknownTTL, errorCooldown: errorCooldown,
		authCooldown: time.Hour, now: now, entries: map[string]entry{}}
}

func (c *Cache) Status(ctx context.Context, q Query) (Status, bool, error) {
	callsign := q.Callsign
	now := c.now()
	c.mu.Lock()
	e, hit := c.entries[callsign]
	c.mu.Unlock()
	if hit && now.Before(e.expires) {
		if e.err {
			return Status{}, false, ErrCooldown
		}
		return e.st, e.ok, nil
	}
	st, ok, err := c.inner.Status(ctx, q)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		cooldown := c.errorCooldown
		if errors.Is(err, ErrAuth) {
			cooldown = c.authCooldown
		}
		c.entries[callsign] = entry{err: true, expires: now.Add(cooldown)}
		return Status{}, false, err
	}
	ttl := c.unknownTTL
	if ok {
		ttl = c.knownTTL
	}
	c.entries[callsign] = entry{st: st, ok: ok, expires: now.Add(ttl)}
	for k, v := range c.entries {
		if !now.Before(v.expires) {
			delete(c.entries, k)
		}
	}
	return st, ok, nil
}
