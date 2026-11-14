package operators

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrCooldown is returned while an aircraft is in its post-error cooldown
// window; the caller should treat the operator as unknown for this tick.
var ErrCooldown = errors.New("operators: lookup in cooldown")

type cacheEntry struct {
	code    string
	ok      bool
	err     bool
	expires time.Time
}

// Cache remembers owner lookups per aircraft so one overhead flight costs at
// most one upstream call. Owners change rarely, hence the long known TTL.
// An upstream error is never stored as a resolved result; it only starts a
// cooldown so a flaky or rate-limited adsbdb is not hammered every tick.
type Cache struct {
	inner         FlagLookup
	knownTTL      time.Duration
	unknownTTL    time.Duration
	errorCooldown time.Duration
	now           func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

func NewCache(inner FlagLookup, knownTTL, unknownTTL, errorCooldown time.Duration, now func() time.Time) *Cache {
	return &Cache{
		inner:         inner,
		knownTTL:      knownTTL,
		unknownTTL:    unknownTTL,
		errorCooldown: errorCooldown,
		now:           now,
		entries:       map[string]cacheEntry{},
	}
}

func (c *Cache) FlagCode(ctx context.Context, hex string) (string, bool, error) {
	key := strings.ToLower(hex)
	now := c.now()
	c.mu.Lock()
	e, hit := c.entries[key]
	c.mu.Unlock()
	if hit && now.Before(e.expires) {
		if e.err {
			return "", false, ErrCooldown
		}
		return e.code, e.ok, nil
	}

	// A spent budget (the tracker's per-tick deadline) is not an upstream
	// failure: skip the call and store no cooldown, so this hex is simply
	// retried next tick instead of being hidden for the cooldown window.
	if err := ctx.Err(); err != nil {
		return "", false, err
	}

	code, ok, err := c.inner.FlagCode(ctx, key)
	if err != nil {
		c.mu.Lock()
		c.entries[key] = cacheEntry{err: true, expires: now.Add(c.errorCooldown)}
		c.mu.Unlock()
		return "", false, err
	}
	ttl := c.unknownTTL
	if ok {
		ttl = c.knownTTL
	}
	c.mu.Lock()
	c.entries[key] = cacheEntry{code: code, ok: ok, expires: now.Add(ttl)}
	// Opportunistic sweep keeps the map bounded without a background goroutine.
	for k, v := range c.entries {
		if !now.Before(v.expires) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
	return code, ok, nil
}
