package flightstatus

import (
	"context"
	"errors"
	"strconv"
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
// whether the route or the delay asked first. Errors enter a short cooldown
// rather than being retried every tick; a refused budget does not.
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

func passKey(q Query) string {
	if q.Seen.IsZero() {
		return q.Callsign
	}
	return q.Callsign + "@" + strconv.FormatInt(q.Seen.Unix(), 10)
}

// Cached returns this pass's stored answer, if any, and never calls
// upstream: it lets the source log see what a paid lookup made for another
// reason (the delay) said, without paying again. found is false for a
// missing, expired or error entry.
func (c *Cache) Cached(q Query) (fl Flight, ok, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.entries[passKey(q)]
	if !hit || e.err || !c.now().Before(e.expires) {
		return Flight{}, false, false
	}
	return e.fl, e.ok, true
}

// Flight serves both Status and route readers from one entry keyed by the
// ident and the pass (first sighting), so a route lookup and a delay lookup
// for the same aircraft share a single paid call. The pass is in the key
// because the chosen instance depends on it; errors stay keyed by ident
// alone, so a cooldown (a bad key above all) is not reset by a new pass.
func (c *Cache) Flight(ctx context.Context, q Query) (Flight, bool, error) {
	callsign, key := q.Callsign, passKey(q)
	now := c.now()
	c.mu.Lock()
	e, hit := c.entries[callsign]
	if !hit || !e.err || !now.Before(e.expires) {
		e, hit = c.entries[key]
	}
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
	if errors.Is(err, ErrBudget) {
		// Not a failure of this ident: asking the budget again is free, and
		// a cached refusal would come back as ErrCooldown, read as an error.
		return Flight{}, false, err
	}
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
	c.entries[key] = entry{fl: fl, ok: ok, expires: now.Add(ttl)}
	for k, v := range c.entries {
		if !now.Before(v.expires) {
			delete(c.entries, k)
		}
	}
	return fl, ok, nil
}
