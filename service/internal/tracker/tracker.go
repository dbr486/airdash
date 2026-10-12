// Package tracker owns the poll loop. Each tick fetches aircraft, resolves
// routes, applies the filter, and publishes the nearest match as a
// Snapshot. Failures keep the previous snapshot so the display degrades
// to "stale" instead of blank.
package tracker

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"airdash/internal/filter"
	"airdash/internal/flightstatus"
	"airdash/internal/geo"
	"airdash/internal/provider"
	"airdash/internal/routes"
)

type Snapshot struct {
	Found       bool
	Hex         string
	Callsign    string
	Type        string
	Reg         string
	Airline     string
	AirlineIATA string
	Origin      string
	Dest        string
	// City names from the route lookup, for the show_names display option.
	OriginName string
	DestName   string
	AltFt      int
	GroundKt   int
	Track      int
	DistKm     float64
	Bearing    int
	Nearby     int
	// Index is the position in the distance-sorted list of matching aircraft; 0 = nearest.
	Index    int
	RadiusKm float64
	Delay    *flightstatus.Status
}

// SettingsSource supplies the filter and radius for each tick, so the web
// UI can change them while the service runs. Optional: when nil, the
// Options values are used unchanged.
type SettingsSource interface {
	Get() (filter.Filter, float64)
	Home() (lat, lon float64, ok bool) // ok false: use the Options position
	DelayWanted() bool                 // paid lookups: show_delay and a cap above zero
	ShowDelay() bool                   // the free board only needs the wish
}

type Options struct {
	Lat, Lon     float64
	RadiusKm     float64
	Poll         time.Duration
	FetchTimeout time.Duration
	RouteTimeout time.Duration
	DelayTimeout time.Duration
	Provider     provider.Provider
	Routes       routes.Lookup
	Delay        flightstatus.Lookup
	// BoardDelay answers from a cached airport board with no network call,
	// so it is asked for every aircraft in the list; Delay (which may chain
	// the same board with a paid lookup) is only asked for the nearest.
	BoardDelay flightstatus.Lookup
	Filter     filter.Filter
	Settings   SettingsSource
	Now        func() time.Time
	Logger     *slog.Logger
}

const (
	backoffAfter    = 3
	backoffInterval = 30 * time.Second
)

// delayOutcome is a tri-state result of one tick's delay lookup:
// delayNoInfo means no lookup ran, or the cache/budget suppressed it
// (cooldown or exhausted budget), and delayFailing must be left as-is
// rather than flapping to "recovered" on every suppressed retry.
type delayOutcome int

const (
	delayNoInfo delayOutcome = iota
	delayOK
	delayFailed
)

type Tracker struct {
	o Options

	mu   sync.RWMutex
	snap Snapshot
	list []Snapshot
	// seen dates each in-range aircraft's first airborne sighting (by hex)
	// so a departure the board has not recorded can be timed; an entry
	// survives a short reception gap and vanishes once the aircraft has been
	// gone for sightingGrace.
	seen         map[string]sighting
	lastSuccess  time.Time
	failures     int
	interval     time.Duration
	routeFailing bool
	delayFailing bool
}

func New(o Options) *Tracker {
	if o.FetchTimeout == 0 {
		o.FetchTimeout = 4 * time.Second
	}
	if o.RouteTimeout == 0 {
		o.RouteTimeout = 3 * time.Second
	}
	if o.DelayTimeout == 0 {
		o.DelayTimeout = 4 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Tracker{o: o, lastSuccess: o.Now(), interval: o.Poll}
}

func (t *Tracker) Current() (Snapshot, time.Time) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snap, t.lastSuccess
}

// List returns every matching aircraft in range from the last successful
// tick, nearest first, so a client can step through them by identity.
func (t *Tracker) List() ([]Snapshot, time.Time) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Snapshot, len(t.list))
	copy(out, t.list)
	return out, t.lastSuccess
}

func (t *Tracker) Interval() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.interval
}

func (t *Tracker) Run(ctx context.Context) {
	for {
		t.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.Interval()):
		}
	}
}

type candidate struct {
	ac      provider.Aircraft
	distKm  float64
	bearing int
	route   routes.Route
	known   bool
}

func (t *Tracker) Tick(ctx context.Context) {
	f, radiusKm := t.o.Filter, t.o.RadiusKm
	lat, lon := t.o.Lat, t.o.Lon
	if t.o.Settings != nil {
		f, radiusKm = t.o.Settings.Get()
		if hl, ho, ok := t.o.Settings.Home(); ok {
			lat, lon = hl, ho
		}
	}
	fctx, cancel := context.WithTimeout(ctx, t.o.FetchTimeout)
	acs, err := t.o.Provider.Fetch(fctx, lat, lon, radiusKm)
	cancel()
	if err != nil {
		// A cancelled caller context (shutdown) is not an upstream
		// failure; skip the failure count and Warn log so a clean stop
		// does not read as an outage in the logs.
		if ctx.Err() != nil {
			return
		}
		t.onFailure(err)
		return
	}
	// Stamped now, before the sequential (up to N x RouteTimeout) route
	// lookups below, so age_s reflects how stale the position data is,
	// not how long route resolution took.
	fetchedAt := t.o.Now()

	var matched []candidate
	nearby := 0
	var routeErr error // first non-cooldown route lookup error this tick, if any
	for _, ac := range acs {
		if ac.OnGround || !ac.HasPos {
			continue
		}
		d := geo.DistanceKm(lat, lon, ac.Lat, ac.Lon)
		if d > radiusKm {
			continue
		}
		c := candidate{ac: ac, distKm: d, bearing: geo.BearingDeg(lat, lon, ac.Lat, ac.Lon)}
		if ac.Callsign != "" {
			rctx, cancel := context.WithTimeout(ctx, t.o.RouteTimeout)
			r, ok, err := t.o.Routes.Route(rctx, ac.Callsign)
			cancel()
			if err != nil {
				t.o.Logger.Debug("route lookup failed", "callsign", ac.Callsign, "err", err)
				// A cooldown hit is a consequence of an earlier failure,
				// not a new one; only a fresh error should (re)trigger the
				// failing/recovered transition below.
				if routeErr == nil && !errors.Is(err, routes.ErrCooldown) {
					routeErr = err
				}
			} else {
				c.route, c.known = r, ok
			}
		}
		if f.Active() && !(c.known && f.Match(c.route)) {
			continue
		}
		nearby++
		matched = append(matched, c)
	}
	// Kept whole and ordered so a client can step through the aircraft in
	// range by identity, not just read the nearest.
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].distKm < matched[j].distKm })
	seen := t.sightings(matched, fetchedAt)
	list := make([]Snapshot, len(matched))
	for i, c := range matched {
		list[i] = toSnapshot(c, nearby, radiusKm)
		list[i].Index = i
	}

	snap := Snapshot{Nearby: nearby, RadiusKm: radiusKm}
	var best *candidate
	if len(matched) > 0 {
		best = &matched[0]
		snap = list[0]
	}

	wantDelay := t.o.Settings != nil && t.o.Settings.DelayWanted()
	if t.o.BoardDelay != nil && t.o.Settings != nil && t.o.Settings.ShowDelay() {
		for i := range list {
			if st, ok, _ := t.o.BoardDelay.Status(ctx, queryFor(matched[i], seen[matched[i].ac.Hex])); ok {
				s := st
				list[i].Delay = &s
			}
		}
		if len(list) > 0 {
			snap.Delay = list[0].Delay
		}
	}

	delayState := delayNoInfo
	var delayErr error
	if best != nil && snap.Delay == nil && t.o.Delay != nil && best.ac.Callsign != "" && wantDelay {
		dctx, cancel := context.WithTimeout(ctx, t.o.DelayTimeout)
		st, ok, err := t.o.Delay.Status(dctx, queryFor(*best, seen[best.ac.Hex]))
		cancel()
		switch {
		case err != nil && (errors.Is(err, flightstatus.ErrCooldown) || errors.Is(err, flightstatus.ErrBudget)):
			// A cooldown/budget refusal is a consequence of an earlier
			// failure (or the daily cap), not fresh information: leave
			// delayState at delayNoInfo so it neither logs a fresh
			// "failing" nor a spurious "recovered".
		case err != nil:
			delayState, delayErr = delayFailed, err
		default:
			delayState = delayOK
			if ok {
				s := st
				snap.Delay = &s
			}
		}
	}

	if best != nil {
		list[0].Delay = snap.Delay
	}
	t.onSuccess(snap, list, fetchedAt, routeErr, delayState, delayErr)
}

func queryFor(c candidate, seen time.Time) flightstatus.Query {
	return flightstatus.Query{Callsign: c.ac.Callsign, AirlineIATA: c.route.AirlineIATA,
		Origin: firstNonEmpty(c.route.OriginIATA, c.route.OriginICAO), Dest: firstNonEmpty(c.route.DestIATA, c.route.DestICAO),
		Seen: seen}
}

type sighting struct{ first, last time.Time }

// sightingGrace is how long an aircraft may drop out of the feed (a weak
// signal right after takeoff, a missed position) before its first sighting
// is forgotten; it comfortably covers a few 5 s polls.
const sightingGrace = 2 * time.Minute

// sightings returns when each matched aircraft was first seen, stamping
// newcomers with now, keeping entries through a brief gap and forgetting
// aircraft gone for longer than sightingGrace.
func (t *Tracker) sightings(matched []candidate, now time.Time) map[string]time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]sighting{}
	}
	// Forget long-gone aircraft first, so one that comes back after the
	// grace period starts a new sighting instead of reviving the old one.
	for hex, s := range t.seen {
		if now.Sub(s.last) > sightingGrace {
			delete(t.seen, hex)
		}
	}
	out := make(map[string]time.Time, len(matched))
	for _, c := range matched {
		s, ok := t.seen[c.ac.Hex]
		if !ok {
			s = sighting{first: now}
		}
		s.last = now
		t.seen[c.ac.Hex] = s
		out[c.ac.Hex] = s.first
	}
	return out
}

func toSnapshot(c candidate, nearby int, radiusKm float64) Snapshot {
	cs := c.ac.Callsign
	if cs == "" {
		cs = c.ac.Reg
	}
	if cs == "" {
		cs = strings.ToUpper(c.ac.Hex)
	}
	return Snapshot{
		Found:       true,
		Hex:         c.ac.Hex,
		Callsign:    cs,
		Type:        c.ac.Type,
		Reg:         c.ac.Reg,
		Airline:     c.route.Airline,
		AirlineIATA: c.route.AirlineIATA,
		Origin:      firstNonEmpty(c.route.OriginIATA, c.route.OriginICAO),
		Dest:        firstNonEmpty(c.route.DestIATA, c.route.DestICAO),
		OriginName:  c.route.OriginName,
		DestName:    c.route.DestName,
		AltFt:       c.ac.AltFt,
		GroundKt:    c.ac.GroundKt,
		Track:       c.ac.Track,
		DistKm:      math.Round(c.distKm*10) / 10,
		Bearing:     c.bearing,
		Nearby:      nearby,
		RadiusKm:    radiusKm,
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (t *Tracker) onSuccess(snap Snapshot, list []Snapshot, fetchedAt time.Time, routeErr error, delayState delayOutcome, delayErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures > 0 {
		t.o.Logger.Info("upstream recovered", "after_failures", t.failures)
	}
	t.failures = 0
	t.interval = t.o.Poll

	failing := routeErr != nil
	switch {
	case failing && !t.routeFailing:
		t.o.Logger.Warn("route lookup failing", "err", routeErr)
	case !failing && t.routeFailing:
		t.o.Logger.Info("route lookup recovered")
	}
	t.routeFailing = failing

	if delayState != delayNoInfo {
		failing := delayState == delayFailed
		switch {
		case failing && !t.delayFailing:
			t.o.Logger.Warn("delay lookup failing", "err", delayErr)
		case !failing && t.delayFailing:
			t.o.Logger.Info("delay lookup recovered")
		}
		t.delayFailing = failing
	}

	switch {
	case snap.Found && snap.Hex != t.snap.Hex:
		t.o.Logger.Info("nearest changed", "callsign", snap.Callsign, "hex", snap.Hex,
			"dist_km", snap.DistKm, "alt_ft", snap.AltFt, "route", snap.Origin+"-"+snap.Dest, "nearby", snap.Nearby)
	case !snap.Found && t.snap.Found:
		t.o.Logger.Info("traffic lost")
	}
	t.snap = snap
	t.list = list
	t.lastSuccess = fetchedAt
}

func (t *Tracker) onFailure(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures++
	if t.failures == 1 {
		t.o.Logger.Warn("upstream fetch failed", "err", err)
	}
	// A 429 means the feed itself asked us to slow down; back off right
	// away instead of waiting for backoffAfter consecutive failures.
	if t.failures >= backoffAfter || errors.Is(err, provider.ErrRateLimited) {
		// Back off to the longer of the two: a configured poll above 30s
		// must not have the failure path poll more often than normal.
		t.interval = max(t.o.Poll, backoffInterval)
	}
}
