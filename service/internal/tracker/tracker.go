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
	"airdash/internal/standing"
)

type Snapshot struct {
	Found    bool
	Hex      string
	Callsign string
	Type     string
	// TypeName is the readable aircraft model ("737-800"), empty when the ICAO code is unmapped.
	TypeName    string
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

// OperatorLookup names the operating airline from the callsign prefix or the
// registry owner. It is the authority over the route lookup's airline because
// adsbdb routes are keyed by callsign alone and can carry a stale or wrong one.
type OperatorLookup interface {
	Operator(ctx context.Context, callsign, hex string, haveAirline bool) (standing.Airline, bool, error)
}

// SettingsSource supplies the filter and radius for each tick, so the web
// UI can change them while the service runs. Optional: when nil, the
// Options values are used unchanged.
type SettingsSource interface {
	Get() (filter.Filter, float64)
	Home() (lat, lon float64, ok bool) // ok false: use the Options position
	DelayWanted() bool                 // paid lookups: show_delay and a cap above zero
	ShowDelay() bool                   // the free board only needs the wish
	PaidRoutesWanted() bool            // route-only AeroAPI calls: paid_routes and a cap above zero
}

// BoardRoute names a route from the cached airport board by exact callsign,
// with no network call.
type BoardRoute interface {
	Route(q flightstatus.Query, now time.Time) (flightstatus.FlightRoute, bool)
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
	Operators    OperatorLookup // nil disables operator resolution
	Delay        flightstatus.Lookup
	// BoardDelay answers from a cached airport board with no network call,
	// so it is asked for every aircraft in the list; Delay (which may chain
	// the same board with a paid lookup) is only asked for the nearest.
	BoardDelay flightstatus.Lookup
	// Flights fills the nearest aircraft's missing route. It must be the same
	// cache instance that sits inside Delay: one entry per ident then serves
	// both, so route and delay never cost two AeroAPI calls. nil disables it.
	Flights flightstatus.FlightLookup
	// BoardRoute is the first route source, ahead of adsbdb; nil = off.
	BoardRoute BoardRoute
	// FallbackRoutes is a free route source (hexdb) asked only when the
	// board and a plausible adsbdb route both miss; nil = off.
	FallbackRoutes routes.Lookup
	// Recorder receives every tick's per-aircraft source answers; nil = off.
	Recorder Recorder
	Filter   filter.Filter
	Settings SettingsSource
	Now      func() time.Time
	Logger   *slog.Logger
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
	// board is the route the airport board gave this tick, to be remembered
	// for the rest of the pass once the aircraft has a sighting.
	board *flightstatus.FlightRoute
	// trace is what each route source said, for the route source log.
	trace trace
	// compare: the fallback source is still to be asked, for the log only.
	compare bool
}

type trace struct {
	board, adsbdb, hexdb, aeroapi SourceAnswer
	shown                         string
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
	// One budget for every owner lookup this tick: positions must not wait on
	// enrichment, and per-call timeouts would add up across aircraft when
	// adsbdb stalls.
	opCtx, opCancel := context.WithTimeout(ctx, t.o.RouteTimeout)
	// The fallback route source gets the same kind of shared budget: it is
	// asked for every aircraft adsbdb cannot name, so a stalled hexdb would
	// otherwise cost one full timeout per aircraft before positions publish.
	// It starts at the first fallback lookup, so slow adsbdb answers before
	// it cannot spend it and push an aircraft on to the paid lookup.
	fallbackCtx, fbCancel := lazyBudget(ctx, t.o.RouteTimeout)
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
		c.trace.adsbdb.Status = StatusNone
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
				c.trace.adsbdb.Status = StatusError
			} else {
				c.route, c.known = r, ok
				c.trace.adsbdb = answerOf(hasRoute(r), routeStr(r))
			}
		}
		t.preferRoute(fallbackCtx, &c, fetchedAt)
		if f.Active() && !(c.known && f.Match(c.route)) {
			continue
		}
		// After the filter so aircraft that are dropped cost no lookup. Runs
		// even with no callsign or no known route: those are exactly the
		// aircraft the route lookup cannot name.
		if t.o.Operators != nil {
			op, ok, err := t.o.Operators.Operator(opCtx, ac.Callsign, ac.Hex, c.route.Airline != "")
			switch {
			case err != nil:
				// Debug only and kept off routeErr: an owner-lookup outage
				// must not flip the route-lookup failing/recovered state.
				t.o.Logger.Debug("operator lookup failed", "callsign", ac.Callsign, "hex", ac.Hex, "err", err)
			case ok:
				c.route.Airline = op.Name
				// The logo is keyed by IATA, so it must follow the name we show:
				// a stale adsbdb airline's code would put another airline's logo
				// next to the VRS name. Keep the route's code only when VRS has none.
				if op.IATA != "" {
					c.route.AirlineIATA = op.IATA
				}
			}
		}
		nearby++
		matched = append(matched, c)
	}
	opCancel()
	fbCancel()
	t.compareFallback(ctx, matched)
	// Kept whole and ordered so a client can step through the aircraft in
	// range by identity, not just read the nearest.
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].distKm < matched[j].distKm })
	seen := t.sightings(matched, fetchedAt)
	for _, c := range matched {
		if c.board != nil {
			t.remember(c.ac.Hex, passRoute{state: passFound, route: *c.board, source: SourceBoard})
		}
	}
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
	var paid paidCall
	if best != nil {
		var changed bool
		changed, paid = t.fillRoute(ctx, best, seen[best.ac.Hex], fetchedAt)
		if changed {
			d := list[0].Delay
			list[0] = toSnapshot(*best, nearby, radiusKm)
			list[0].Index, list[0].Delay = 0, d
			snap = list[0]
		}
	}
	if paid.called {
		// The same cache answers the delay path, so asking it again would
		// only replay this result; take the delay from here instead.
		delayState, delayErr = classifyDelay(paid.err)
		if paid.err == nil && wantDelay && snap.Delay == nil && paid.fl.HasStatus {
			s := paid.fl.Status
			snap.Delay = &s
		}
	} else if ident := t.delayIdent(best); ident != "" && snap.Delay == nil && t.o.Delay != nil && wantDelay &&
		!t.passMemo(best.ac.Hex, fetchedAt).noFlight {
		dctx, cancel := context.WithTimeout(ctx, t.o.DelayTimeout)
		dq := queryFor(*best, seen[best.ac.Hex])
		dq.Callsign = ident
		st, ok, err := t.o.Delay.Status(dctx, dq)
		cancel()
		delayState, delayErr = classifyDelay(err)
		// A failed paid call leaves no cache entry for paidAnswer to read.
		switch {
		case errors.Is(err, flightstatus.ErrBudget):
			best.trace.aeroapi.Status = StatusSkipped
		case err != nil:
			best.trace.aeroapi.Status = StatusError
		}
		if err == nil && ok {
			s := st
			snap.Delay = &s
		}
	}

	if best != nil {
		list[0].Delay = snap.Delay
	}
	if t.o.Recorder != nil && len(matched) > 0 {
		t.paidAnswer(best, seen)
		t.o.Recorder.Record(passRecords(matched, seen, fetchedAt))
	}
	t.onSuccess(snap, list, fetchedAt, routeErr, delayState, delayErr)
}

// cachedFlights is the paid lookup's cache seen without calling upstream.
type cachedFlights interface {
	Cached(q flightstatus.Query) (fl flightstatus.Flight, ok, found bool)
}

// paidAnswer fills in what AeroAPI said this pass when the call was made for
// the delay rather than the route (the shown route came from a free source),
// by reading the shared cache entry: it never pays for the log.
func (t *Tracker) paidAnswer(best *candidate, seen map[string]time.Time) {
	cf, ok := t.o.Flights.(cachedFlights)
	if !ok || best == nil || best.trace.aeroapi.Status != StatusNotAsked {
		return
	}
	ident := t.delayIdent(best)
	if ident == "" {
		return
	}
	q := queryFor(*best, seen[best.ac.Hex])
	q.Callsign = ident
	fl, ok, found := cf.Cached(q)
	switch {
	case !found:
	case ok && fl.HasRoute:
		best.trace.aeroapi = SourceAnswer{Status: StatusHit, Route: flightRouteStr(fl.Route)}
	default:
		best.trace.aeroapi = SourceAnswer{Status: StatusUnknown}
	}
}

func passRecords(matched []candidate, seen map[string]time.Time, now time.Time) []PassRecord {
	recs := make([]PassRecord, len(matched))
	for i, c := range matched {
		r := PassRecord{
			Hex: c.ac.Hex, Callsign: c.ac.Callsign, Reg: c.ac.Reg, Type: c.ac.Type, Airline: c.route.Airline,
			FirstSeen: seen[c.ac.Hex], LastSeen: now,
			DistKm: math.Round(c.distKm*10) / 10, AltFt: c.ac.AltFt, Nearest: i == 0,
			Board: c.trace.board, ADSBDB: c.trace.adsbdb, HexDB: c.trace.hexdb, AeroAPI: c.trace.aeroapi,
			ShownSource: c.trace.shown,
		}
		if r.ShownSource != SourceNone {
			r.ShownRoute = routeStr(c.route)
		}
		recs[i] = r
	}
	return recs
}

// delayIdent is the cache key for the nearest aircraft's delay lookup, keyed
// like the paid route call so both share one entry (a lower-case or padded
// callsign would otherwise miss it and cost a second paid call). Without a
// callsign the registration is used only when paid routes are on and the
// aircraft is one fillRoute would pay for: that is the only way a
// registration lookup is authorised, and it lets a route found by
// registration keep its delay on later ticks, from the same cache entry.
func (t *Tracker) delayIdent(best *candidate) string {
	if best == nil {
		return ""
	}
	if id := flightstatus.Ident(best.ac.Callsign, ""); id != "" {
		return id
	}
	if t.o.Settings == nil || !t.o.Settings.PaidRoutesWanted() || light(best.ac) {
		return ""
	}
	return flightstatus.Ident("", best.ac.Reg)
}

func classifyDelay(err error) (delayOutcome, error) {
	switch {
	case err != nil && (errors.Is(err, flightstatus.ErrCooldown) || errors.Is(err, flightstatus.ErrBudget)):
		// A cooldown/budget refusal is a consequence of an earlier
		// failure (or the daily cap), not fresh information: report
		// delayNoInfo so it neither logs a fresh "failing" nor a
		// spurious "recovered".
		return delayNoInfo, nil
	case err != nil:
		return delayFailed, err
	}
	return delayOK, nil
}

// paidCall is the outcome of this tick's route-only AeroAPI call, if any.
type paidCall struct {
	called bool
	fl     flightstatus.Flight
	err    error
}

// fillRoute gives the nearest aircraft a route with one AeroAPI call when
// preferRoute left it none. Filters already ran, so a route found here can
// change what is shown, never what is listed.
func (t *Tracker) fillRoute(ctx context.Context, c *candidate, seen, now time.Time) (changed bool, paid paidCall) {
	if hasRoute(c.route) {
		return false, paid
	}
	hex := c.ac.Hex
	if t.passMemo(hex, now).state == passUnknown {
		return false, paid
	}
	if t.o.Flights == nil || t.o.Settings == nil || !t.o.Settings.PaidRoutesWanted() || light(c.ac) {
		return false, paid
	}
	ident := flightstatus.Ident(c.ac.Callsign, c.ac.Reg)
	if ident == "" {
		return false, paid
	}
	q := queryFor(*c, seen)
	q.Callsign = ident // the cache key, shared with the delay lookup
	fctx, cancel := context.WithTimeout(ctx, t.o.DelayTimeout)
	fl, ok, err := t.o.Flights.Flight(fctx, q)
	cancel()
	paid = paidCall{called: true, fl: fl, err: err}
	switch {
	case errors.Is(err, flightstatus.ErrBudget):
		c.trace.aeroapi.Status = StatusSkipped
	case err != nil:
		c.trace.aeroapi.Status = StatusError
		// A refusal or an outage says nothing about this flight; the
		// cache's cooldown already spaces out the retries.
		t.o.Logger.Debug("paid route lookup failed", "ident", ident, "err", err)
	case ok && fl.HasRoute:
		applyRoute(c, fl.Route)
		t.remember(hex, passRoute{state: passFound, route: fl.Route, source: SourceAeroAPI})
		c.trace.aeroapi = SourceAnswer{Status: StatusHit, Route: flightRouteStr(fl.Route)}
		c.trace.shown = SourceAeroAPI
		changed = true
	default:
		c.trace.aeroapi.Status = StatusUnknown
		// Remembered for the whole pass, not the cache's 15 minutes: an
		// aircraft AeroAPI cannot name now will not become nameable
		// overhead, and each retry would spend the shared daily cap.
		t.remember(hex, passRoute{state: passUnknown, noFlight: !ok})
	}
	return changed, paid
}

// preferRoute settles which route an aircraft shows, before the filter sees
// it. A route the board or AeroAPI gave earlier in this pass comes first, so
// it cannot flap mid-pass. Then the airport board: it lists today's flights
// with their times, while adsbdb keeps whatever a flight number last flew,
// and airlines reuse numbers on other city pairs. adsbdb's route is kept only if the aircraft's
// position could lie on it; otherwise its ends are dropped (the airline from
// the callsign prefix stays). A route still missing is asked of the free
// fallback source under the same position check, and only then may
// fillRoute pay AeroAPI.
func (t *Tracker) preferRoute(fallbackCtx func() context.Context, c *candidate, now time.Time) {
	// Every source is asked (the board and cached hexdb cost no upstream
	// call) so the source log can compare them; which one is shown is
	// decided afterwards, in the order above.
	boardRoute, boardOK := t.askBoard(c, now)
	plausible := hasRoute(c.route) && c.route.Plausible(c.ac.Lat, c.ac.Lon)
	if hasRoute(c.route) && !plausible {
		c.trace.adsbdb.Status = StatusImplausible
	}
	m := t.passMemo(c.ac.Hex, now)
	c.trace.aeroapi = t.aeroAnswer(m)
	var hexRoute routes.Route
	var hexOK bool
	if m.state == passFound || boardOK || plausible {
		// Named already: hexdb's answer is only for the source log, asked
		// after every aircraft that needs it (compareFallback).
		c.compare = true
	} else {
		hexRoute, hexOK = t.askFallback(fallbackCtx, c)
	}

	switch {
	case m.state == passFound:
		useRoute(c, m.route)
		c.trace.shown = m.source
	case boardOK:
		useRoute(c, boardRoute)
		c.board = &boardRoute
		c.trace.shown = SourceBoard
	case plausible:
		c.trace.shown = SourceADSBDB
	default:
		// Dropped only now: a board or remembered route naming the same
		// airports keeps adsbdb's city names (useRoute above).
		if hasRoute(c.route) {
			t.o.Logger.Debug("implausible route dropped", "callsign", c.ac.Callsign, "route", routeStr(c.route))
			c.route = routes.Route{Airline: c.route.Airline, AirlineIATA: c.route.AirlineIATA}
		}
		t.useFallback(c, hexRoute, hexOK)
	}
}

// useFallback shows the fallback route, if it found one, in place of the
// dropped or missing adsbdb route.
func (t *Tracker) useFallback(c *candidate, hexRoute routes.Route, hexOK bool) {
	switch {
	case hexOK:
		hexRoute.Airline, hexRoute.AirlineIATA = c.route.Airline, c.route.AirlineIATA
		c.route, c.known = hexRoute, true
		c.trace.shown = SourceHexDB
	default:
		c.trace.shown = SourceNone
	}
}

// aeroAnswer is AeroAPI's answer before this tick's paid call, if any: what
// it said earlier in the pass, else not asked. fillRoute updates it.
func (t *Tracker) aeroAnswer(m passRoute) SourceAnswer {
	switch {
	case t.o.Flights == nil:
		return SourceAnswer{Status: StatusOff}
	case m.state == passFound && m.source == SourceAeroAPI:
		return SourceAnswer{Status: StatusHit, Route: flightRouteStr(m.route)}
	case m.state == passUnknown:
		return SourceAnswer{Status: StatusUnknown}
	}
	return SourceAnswer{Status: StatusNotAsked}
}

func (t *Tracker) askBoard(c *candidate, now time.Time) (flightstatus.FlightRoute, bool) {
	switch {
	case t.o.BoardRoute == nil:
		c.trace.board.Status = StatusOff
	case c.ac.Callsign == "":
		c.trace.board.Status = StatusNone
	default:
		r, ok := t.o.BoardRoute.Route(queryFor(*c, time.Time{}), now)
		if ok {
			c.trace.board = SourceAnswer{Status: StatusHit, Route: flightRouteStr(r)}
			return r, true
		}
		c.trace.board.Status = StatusMiss
	}
	return flightstatus.FlightRoute{}, false
}

// askFallback asks the free fallback source (hexdb) and reports a route the
// aircraft could be flying. Its errors stay at debug level and off the
// failing/recovered state, which tracks adsbdb: a fallback outage only means
// fewer routes, never stale ones. The context is the tick's shared fallback
// budget; once it is spent the cache still answers from memory but makes no
// upstream call.
func (t *Tracker) askFallback(fallbackCtx func() context.Context, c *candidate) (routes.Route, bool) {
	switch {
	case t.o.FallbackRoutes == nil:
		c.trace.hexdb.Status = StatusOff
		return routes.Route{}, false
	case c.ac.Callsign == "":
		c.trace.hexdb.Status = StatusNone
		return routes.Route{}, false
	}
	ctx := fallbackCtx()
	r, ok, err := t.o.FallbackRoutes.Route(ctx, c.ac.Callsign)
	switch {
	case err != nil && ctx.Err() != nil:
		c.trace.hexdb.Status = StatusSkipped
	case err != nil:
		c.trace.hexdb.Status = StatusError
		t.o.Logger.Debug("fallback route lookup failed", "callsign", c.ac.Callsign, "err", err)
	case !ok:
		c.trace.hexdb.Status = StatusUnknown
	case !r.Plausible(c.ac.Lat, c.ac.Lon):
		c.trace.hexdb = SourceAnswer{Status: StatusImplausible, Route: routeStr(r)}
	default:
		c.trace.hexdb = SourceAnswer{Status: StatusHit, Route: routeStr(r)}
		return r, true
	}
	return routes.Route{}, false
}

// lazyBudget returns a shared deadline that starts at its first use, and
// the cancel for it (a no-op if it was never used).
func lazyBudget(parent context.Context, d time.Duration) (get func() context.Context, cancel func()) {
	var ctx context.Context
	stop := context.CancelFunc(func() {})
	get = func() context.Context {
		if ctx == nil {
			ctx, stop = context.WithTimeout(parent, d)
		}
		return ctx
	}
	return get, func() { stop() }
}

// compareFallback asks the fallback source, for the source log only, about
// listed aircraft that were named without it. It runs after every lookup the
// display needs and on a budget of its own, so a stall here can neither
// starve those lookups nor push an aircraft on to the paid one; without a
// recorder it costs nothing.
func (t *Tracker) compareFallback(ctx context.Context, matched []candidate) {
	if t.o.Recorder == nil {
		for i := range matched {
			if matched[i].compare {
				matched[i].trace.hexdb.Status = StatusNotAsked
			}
		}
		return
	}
	budget, cancel := lazyBudget(ctx, t.o.RouteTimeout)
	defer cancel()
	for i := range matched {
		if matched[i].compare {
			t.askFallback(budget, &matched[i])
		}
	}
}

func answerOf(found bool, route string) SourceAnswer {
	if !found {
		return SourceAnswer{Status: StatusUnknown}
	}
	return SourceAnswer{Status: StatusHit, Route: route}
}

func routeStr(r routes.Route) string {
	if !hasRoute(r) {
		return ""
	}
	return firstNonEmpty(r.OriginIATA, r.OriginICAO) + "-" + firstNonEmpty(r.DestIATA, r.DestICAO)
}

func flightRouteStr(r flightstatus.FlightRoute) string {
	return routeStr(routes.Route{OriginIATA: r.OriginIATA, OriginICAO: r.OriginICAO, DestIATA: r.DestIATA, DestICAO: r.DestICAO})
}

// useRoute puts a board or AeroAPI route on the candidate. When it names the
// same airports as adsbdb, adsbdb's route is kept for its city names.
func useRoute(c *candidate, r flightstatus.FlightRoute) {
	c.known = true
	if sameEnd(c.route.OriginIATA, c.route.OriginICAO, r.OriginIATA, r.OriginICAO) &&
		sameEnd(c.route.DestIATA, c.route.DestICAO, r.DestIATA, r.DestICAO) {
		return
	}
	applyRoute(c, r)
}

// sameEnd compares two airports by whichever code both sides have.
func sameEnd(iataA, icaoA, iataB, icaoB string) bool {
	switch {
	case iataA != "" && iataB != "":
		return iataA == iataB
	case icaoA != "" && icaoB != "":
		return icaoA == icaoB
	}
	return false
}

// cityOr keeps the source's city name and otherwise names the airport from
// the embedded table: hexdb and the board send codes only, and without this
// the show_names option would show codes for a third of routes.
func cityOr(name, iata, icao string) string {
	if name != "" {
		return name
	}
	return standing.AirportCity(iata, icao)
}

func hasRoute(r routes.Route) bool {
	return firstNonEmpty(r.OriginIATA, r.OriginICAO) != "" || firstNonEmpty(r.DestIATA, r.DestICAO) != ""
}

// light reports aircraft not worth a paid lookup: local general aviation
// files no flight AeroAPI could find. A1 is the "light" emitter category; an
// aircraft with neither a category nor a callsign is almost always the same.
func light(ac provider.Aircraft) bool {
	return ac.Category == "A1" || (ac.Category == "" && ac.Callsign == "")
}

// applyRoute fills the route ends from a board or AeroAPI route. The airline
// is only filled when still empty: the callsign/registry operator is the
// better authority, and the logo code must follow the name shown.
func applyRoute(c *candidate, r flightstatus.FlightRoute) {
	c.route.OriginIATA, c.route.OriginICAO, c.route.OriginName = r.OriginIATA, r.OriginICAO, r.OriginCity
	c.route.DestIATA, c.route.DestICAO, c.route.DestName = r.DestIATA, r.DestICAO, r.DestCity
	if c.route.Airline == "" && r.Operator != "" {
		c.route.Airline = r.Operator
		if r.OperatorIATA != "" {
			c.route.AirlineIATA = r.OperatorIATA
		}
	}
}

func queryFor(c candidate, seen time.Time) flightstatus.Query {
	return flightstatus.Query{Callsign: c.ac.Callsign, AirlineIATA: c.route.AirlineIATA,
		Origin: firstNonEmpty(c.route.OriginIATA, c.route.OriginICAO), Dest: firstNonEmpty(c.route.DestIATA, c.route.DestICAO),
		Seen: seen, Reg: c.ac.Reg}
}

// sighting is one pass of an aircraft through range. The route memory rides
// on it so it is forgotten exactly when the pass ends.
type sighting struct {
	first, last time.Time
	route       passRoute
}

type passState int

const (
	passNone passState = iota
	passFound
	passUnknown
)

// passRoute is what the board or AeroAPI said about the route during this
// pass. A found route outlives the 15-minute flightstatus cache entry so it
// does not vanish mid-pass; an unknown one caps the cost at one call.
type passRoute struct {
	state passState
	route flightstatus.FlightRoute
	// source is the route source that found route, kept for the source log.
	source string
	// noFlight: AeroAPI has no such flight at all, so it has no delay either
	// and the delay path must not pay to ask again once the cache expires.
	noFlight bool
}

// passMemo is read before sightings() forgets long-gone aircraft, so it
// applies the same grace itself: a returning aircraft starts a new pass.
func (t *Tracker) passMemo(hex string, now time.Time) passRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.seen[hex]
	if !ok || now.Sub(s.last) > sightingGrace {
		return passRoute{}
	}
	return s.route
}

func (t *Tracker) remember(hex string, r passRoute) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.seen[hex]; ok {
		s.route = r
		t.seen[hex] = s
	}
}

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
		TypeName:    standing.TypeName(c.ac.Type),
		Reg:         c.ac.Reg,
		Airline:     c.route.Airline,
		AirlineIATA: c.route.AirlineIATA,
		Origin:      firstNonEmpty(c.route.OriginIATA, c.route.OriginICAO),
		Dest:        firstNonEmpty(c.route.DestIATA, c.route.DestICAO),
		OriginName:  cityOr(c.route.OriginName, c.route.OriginIATA, c.route.OriginICAO),
		DestName:    cityOr(c.route.DestName, c.route.DestIATA, c.route.DestICAO),
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
			t.o.Logger.Warn("AeroAPI lookup failing", "err", delayErr)
		case !failing && t.delayFailing:
			t.o.Logger.Info("AeroAPI lookup recovered")
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
