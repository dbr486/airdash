package routes

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrRateLimited is returned, without any network I/O, while the adsbdb
// gate is closed. It wraps ErrCooldown: a rejection is a consequence of the
// 429 that closed the gate, not a fresh failure.
var ErrRateLimited = fmt.Errorf("adsbdb: rate limited, backing off: %w", ErrCooldown)

const (
	// adsbdb's longest ban, and what its 429s say once the per-IP counter
	// has passed 1024. It answers 429s without a Retry-After header, and its
	// body (which states the length) is not read: a 429 that flushes its
	// headers and stalls the body would otherwise hold up the tracker tick.
	// Waiting the longest ban also covers its shorter 60 s one.
	defaultRateLimit = 300 * time.Second
	// Keeps the first probe clear of the ban's TTL edge: a probe that
	// lands a moment early would restart the ban.
	rateLimitMargin  = 15 * time.Second
	maxRateLimitWait = time.Hour
)

// Gate stops all traffic to adsbdb after a 429 until the ban has run out.
//
// adsbdb rate-limits per IP with a counter that every request bumps,
// rejected ones included, and past 1024 each request resets a 300 s ban.
// Per-callsign retries therefore keep the ban alive forever; only a
// stretch of total silence from this IP, across route and owner lookups,
// lifts it. So one Gate is shared by every adsbdb client, and while it is
// closed requests fail at once, adding no latency to the tracker tick.
// After a ban, one probe goes out at a time until a non-429 answer.
type Gate struct {
	now    func() time.Time
	logger *slog.Logger

	mu      sync.Mutex
	until   time.Time
	streak  int  // 429s since the last other answer
	probing bool // a request is in flight while streak > 0
}

func NewGate(now func() time.Time, logger *slog.Logger) *Gate {
	return &Gate{now: now, logger: logger}
}

// Wrap returns a copy of c whose requests pass through the gate.
func (g *Gate) Wrap(c *http.Client) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp := *c
	cp.Transport = &gateTransport{gate: g, base: base}
	return &cp
}

type gateTransport struct {
	gate *Gate
	base http.RoundTripper
}

func (t *gateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g := t.gate
	ok, probe := g.admit()
	if !ok {
		if req.Body != nil {
			req.Body.Close() // RoundTrip must close it, even on error
		}
		return nil, ErrRateLimited
	}
	resp, err := t.base.RoundTrip(req)
	g.done(resp, err, probe)
	return resp, err
}

// admit reports whether a request may go out now, and whether it holds the
// single probe slot claimed when the gate has just reopened after a ban.
func (g *Gate) admit() (ok, probe bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.now().Before(g.until) {
		return false, false
	}
	if g.streak > 0 {
		if g.probing {
			return false, false
		}
		g.probing = true
		return true, true
	}
	return true, false
}

// done records the outcome of an admitted request. It runs as soon as the
// headers are in, so the gate closes before the 429's body is read.
func (g *Gate) done(resp *http.Response, err error, probe bool) {
	g.mu.Lock()
	if probe {
		g.probing = false
	}
	if err != nil {
		// A timeout or DNS failure says nothing about the ban.
		g.mu.Unlock()
		return
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		// Only an answer after the window proves the ban is over; one
		// admitted before the ban and landing during it proves nothing.
		if !g.now().Before(g.until) {
			g.streak = 0
		}
		g.mu.Unlock()
		return
	}
	g.streak++
	streak := g.streak
	// Doubling covers a ban that outlives what it advertised, e.g. when
	// something else on the shared public IP keeps feeding the counter.
	wait := min(retryAfter(resp.Header)<<min(streak-1, 10), maxRateLimitWait)
	wait = min(wait+rateLimitMargin, maxRateLimitWait)
	// Never shorten a window: a concurrent request admitted before this
	// 429 may answer after a longer one was set.
	if until := g.now().Add(wait); until.After(g.until) {
		g.until = until
	}
	g.mu.Unlock()
	g.logger.Warn("adsbdb rate limited; pausing all adsbdb lookups", "wait", wait, "streak", streak)
}

// retryAfter reads a Retry-After header in seconds, else the default. It
// is capped so the doubling in done cannot overflow into a negative wait.
func retryAfter(h http.Header) time.Duration {
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil && n > 0 {
		return time.Duration(min(n, int(maxRateLimitWait/time.Second))) * time.Second
	}
	return defaultRateLimit
}
