package flightstatus

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCooldown = errors.New("flightstatus: lookup in cooldown")

type entry struct {
	fl      Flight
	ok      bool
	err     bool
	expires time.Time
}

// Cache remembers the flight (route and status together) per ident, so
// the aircraft overhead for ten minutes costs one AeroAPI result set
// whether the route or the delay asked first. Errors (including a refused
// budget) enter a short cooldown rather than being retried every tick.
type Cache struct {
	inner         FlightLookup
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

func NewCache(inner FlightLookup, knownTTL, unknownTTL, errorCooldown time.Duration, now func() time.Time) *Cache {
	return &Cache{inner: inner, knownTTL: knownTTL, unknownTTL: unknownTTL, errorCooldown: errorCooldown,
		authCooldown: time.Hour, now: now, entries: map[string]entry{}}
}

func (c *Cache) Status(ctx context.Context, q Query) (Status, bool, error) {
	f, ok, err := c.Flight(ctx, q)
	return statusOf(f, ok, err)
}

// Flight serves both Status and route readers from one entry keyed by the
// ident, so a route lookup and a delay lookup for the same aircraft share
// a single paid call.
func (c *Cache) Flight(ctx context.Context, q Query) (Flight, bool, error) {
	callsign := q.Callsign
	now := c.now()
	c.mu.Lock()
	e, hit := c.entries[callsign]
	c.mu.Unlock()
	if hit && now.Before(e.expires) {
		if e.err {
			return Flight{}, false, ErrCooldown
		}
		return e.fl, e.ok, nil
	}
	fl, ok, err := c.inner.Flight(ctx, q)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		cooldown := c.errorCooldown
		if errors.Is(err, ErrAuth) {
			cooldown = c.authCooldown
		}
		c.entries[callsign] = entry{err: true, expires: now.Add(cooldown)}
		return Flight{}, false, err
	}
	ttl := c.unknownTTL
	if ok {
		ttl = c.knownTTL
	}
	c.entries[callsign] = entry{fl: fl, ok: ok, expires: now.Add(ttl)}
	for k, v := range c.entries {
		if !now.Before(v.expires) {
			delete(c.entries, k)
		}
	}
	return fl, ok, nil
}
