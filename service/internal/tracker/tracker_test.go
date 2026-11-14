package tracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airdash/internal/filter"
	"airdash/internal/flightstatus"
	"airdash/internal/provider"
	"airdash/internal/routes"
	"airdash/internal/settings"
	"airdash/internal/standing"
)

// Home is Heathrow-ish. Distances below are approximate and only need
// to order correctly.
const homeLat, homeLon = 51.47, -0.45

type fakeProvider struct {
	acs      []provider.Aircraft
	err      error
	lat, lon float64 // last requested centre
}

func (f *fakeProvider) Fetch(ctx context.Context, lat, lon, r float64) ([]provider.Aircraft, error) {
	f.lat, f.lon = lat, lon
	return f.acs, f.err
}

type fakeRoutes struct {
	byCallsign map[string]routes.Route
	errFor     map[string]bool
	errAll     error // when set, every lookup fails with it
	calls      int
}

func (f *fakeRoutes) Route(ctx context.Context, cs string) (routes.Route, bool, error) {
	f.calls++
	if f.errAll != nil {
		return routes.Route{}, false, f.errAll
	}
	if f.errFor[cs] {
		return routes.Route{}, false, errors.New("lookup failed")
	}
	r, ok := f.byCallsign[cs]
	return r, ok, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func airborne(hex, cs string, lat, lon float64) provider.Aircraft {
	return provider.Aircraft{Hex: hex, Callsign: cs, Type: "A320", Reg: "G-" + hex, Lat: lat, Lon: lon, HasPos: true, AltFt: 3000, GroundKt: 250, Track: 90}
}

func newTracker(p provider.Provider, r routes.Lookup, f filter.Filter, ck *clock) *Tracker {
	return New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: r, Filter: f, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestTick_PicksNearestAndCountsNearby(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{
		airborne("aaa", "FAR1", 51.53, -0.45),  // ~6.7 km north
		airborne("bbb", "NEAR1", 51.48, -0.45), // ~1.1 km north
		airborne("ccc", "MID1", 51.50, -0.45),  // ~3.3 km north
	}}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(p, &fakeRoutes{}, filter.Filter{}, ck)

	tr.Tick(t.Context())
	s, at := tr.Current()
	if !s.Found || s.Hex != "bbb" || s.Callsign != "NEAR1" {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.Nearby != 3 {
		t.Errorf("Nearby = %d, want 3", s.Nearby)
	}
	if s.DistKm < 1.0 || s.DistKm > 1.2 {
		t.Errorf("DistKm = %v", s.DistKm)
	}
	if s.Bearing > 1 && s.Bearing < 359 {
		t.Errorf("Bearing = %d, want ~0", s.Bearing)
	}
	if !at.Equal(ck.t) {
		t.Errorf("last success = %v, want %v", at, ck.t)
	}
}

func TestTick_SkipsGroundAndNoPosition(t *testing.T) {
	ground := airborne("ggg", "GND1", 51.4701, -0.45)
	ground.OnGround = true
	nopos := airborne("nnn", "NOPOS", 0, 0)
	nopos.HasPos = false
	p := &fakeProvider{acs: []provider.Aircraft{ground, nopos, airborne("bbb", "NEAR1", 51.48, -0.45)}}
	ck := &clock{t: time.Now()}
	tr := newTracker(p, &fakeRoutes{}, filter.Filter{}, ck)

	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Hex != "bbb" || s.Nearby != 1 {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestTick_RadiusIsKmNotNm(t *testing.T) {
	// 10.5 km north: inside the 6 nm (11.1 km) upstream box, outside 10 km.
	p := &fakeProvider{acs: []provider.Aircraft{airborne("out", "OUT1", 51.5645, -0.45)}}
	ck := &clock{t: time.Now()}
	tr := newTracker(p, &fakeRoutes{}, filter.Filter{}, ck)

	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Found || s.Nearby != 0 {
		t.Errorf("aircraft outside km radius must be excluded: %+v", s)
	}
}

func TestTick_CallsignFallback(t *testing.T) {
	noCs := airborne("abc123", "", 51.48, -0.45)
	noCs.Reg = "G-TEST"
	noCsNoReg := airborne("def456", "", 51.49, -0.45)
	noCsNoReg.Reg = ""

	ck := &clock{t: time.Now()}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{noCs}}, &fakeRoutes{}, filter.Filter{}, ck)
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Callsign != "G-TEST" {
		t.Errorf("want registration fallback, got %q", s.Callsign)
	}

	tr2 := newTracker(&fakeProvider{acs: []provider.Aircraft{noCsNoReg}}, &fakeRoutes{}, filter.Filter{}, ck)
	tr2.Tick(t.Context())
	if s, _ := tr2.Current(); s.Callsign != "DEF456" {
		t.Errorf("want upper-cased hex fallback, got %q", s.Callsign)
	}
}

func TestTick_RoutesAreAttached(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{
		"BAW466":   {Airline: "British Airways", AirlineIATA: "BA", OriginIATA: "LHR", OriginICAO: "EGLL", DestIATA: "BCN", DestICAO: "LEBL", OriginName: "London", DestName: "Barcelona"},
		"ICAOONLY": {OriginICAO: "EGLL", DestICAO: "LEBL"},
	}}
	ck := &clock{t: time.Now()}

	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "BAW466", 51.48, -0.45)}}, r, filter.Filter{}, ck)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Airline != "British Airways" || s.Origin != "LHR" || s.Dest != "BCN" || s.OriginName != "London" || s.DestName != "Barcelona" || s.AirlineIATA != "BA" {
		t.Errorf("route not attached: %+v", s)
	}

	tr2 := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("b", "ICAOONLY", 51.48, -0.45)}}, r, filter.Filter{}, ck)
	tr2.Tick(t.Context())
	s2, _ := tr2.Current()
	if s2.Origin != "EGLL" || s2.Dest != "LEBL" {
		t.Errorf("ICAO fallback not applied: %+v", s2)
	}
}

func TestTick_NoFilterShowsUnknownRoute(t *testing.T) {
	ck := &clock{t: time.Now()}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "PRIV1", 51.48, -0.45)}}, &fakeRoutes{}, filter.Filter{}, ck)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if !s.Found || s.Origin != "" || s.Airline != "" {
		t.Errorf("unknown route should still show with empty fields: %+v", s)
	}
}

func TestTick_FilterHidesUnknownAndErrored(t *testing.T) {
	f, err := filter.Parse("LHR", "", "")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRoutes{
		byCallsign: map[string]routes.Route{
			"MATCH1": {OriginIATA: "LHR", DestIATA: "BCN"},
			"OTHER1": {OriginIATA: "LGW", DestIATA: "AMS"},
		},
		errFor: map[string]bool{"ERR1": true},
	}
	p := &fakeProvider{acs: []provider.Aircraft{
		airborne("e", "ERR1", 51.475, -0.45),  // nearest, lookup errors
		airborne("u", "UNKN1", 51.478, -0.45), // unknown route
		airborne("o", "OTHER1", 51.48, -0.45), // known, does not match
		airborne("m", "MATCH1", 51.49, -0.45), // matches
		airborne("n", "", 51.476, -0.45),      // no callsign at all
	}}
	ck := &clock{t: time.Now()}
	tr := newTracker(p, r, f, ck)

	tr.Tick(t.Context())
	s, at := tr.Current()
	if !s.Found || s.Hex != "m" {
		t.Fatalf("expected MATCH1, got %+v", s)
	}
	if s.Nearby != 1 {
		t.Errorf("Nearby counts matching only, got %d", s.Nearby)
	}
	if !at.Equal(ck.t) {
		t.Error("a route lookup error must not fail the tick")
	}
}

// fakeRoutesAdvancing simulates a route lookup that takes real time: each
// call advances the fake clock, so a tick with N sequential lookups would
// stamp lastSuccess N steps late unless the tracker captures the fetch
// time before doing route lookups.
type fakeRoutesAdvancing struct {
	byCallsign map[string]routes.Route
	step       time.Duration
	ck         *clock
}

func (f *fakeRoutesAdvancing) Route(ctx context.Context, cs string) (routes.Route, bool, error) {
	f.ck.t = f.ck.t.Add(f.step)
	r, ok := f.byCallsign[cs]
	return r, ok, nil
}

func TestTick_LastSuccessIsFetchTime(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{
		airborne("aaa", "FAR1", 51.48, -0.45),
		airborne("bbb", "NEAR1", 51.49, -0.45),
	}}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	r := &fakeRoutesAdvancing{byCallsign: map[string]routes.Route{}, step: 2 * time.Second, ck: ck}
	tr := newTracker(p, r, filter.Filter{}, ck)

	fetchedAt := ck.t
	tr.Tick(t.Context())
	_, at := tr.Current()
	if !at.Equal(fetchedAt) {
		t.Errorf("last success = %v, want fetch time %v (route lookups advanced clock to %v)", at, fetchedAt, ck.t)
	}
}

// recordingHandler is a minimal slog.Handler that appends each record's
// message to a slice, so tests can assert on log call counts without
// parsing text/JSON output.
type recordingHandler struct {
	mu   *sync.Mutex
	msgs *[]string
}

func newRecordingHandler() (*recordingHandler, *[]string) {
	msgs := &[]string{}
	return &recordingHandler{mu: &sync.Mutex{}, msgs: msgs}, msgs
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func countMsg(msgs []string, want string) int {
	n := 0
	for _, m := range msgs {
		if m == want {
			n++
		}
	}
	return n
}

// A suppressed lookup (cooldown, or the adsbdb gate during a ban) carries no
// news about the upstream, so it must neither log "recovered" while the ban
// lasts nor a second "failing" when the next probe is refused again.
func TestTick_SuppressedRouteLookupsKeepFailingState(t *testing.T) {
	h, msgs := newRecordingHandler()
	ac := airborne("a", "RYR1HU", 51.48, -0.45)
	p := &fakeProvider{acs: []provider.Aircraft{ac}}
	r := &fakeRoutes{errAll: errors.New("adsbdb: status 429")}
	ck := &clock{t: time.Now()}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: r, Filter: filter.Filter{}, Now: ck.now,
		Logger: slog.New(h),
	})

	tr.Tick(t.Context())
	r.errAll = routes.ErrRateLimited
	tr.Tick(t.Context())
	tr.Tick(t.Context())
	r.errAll = errors.New("adsbdb: status 429")
	tr.Tick(t.Context())
	if got := countMsg(*msgs, "route lookup recovered"); got != 0 {
		t.Errorf(`"route lookup recovered" logged %d times during the ban, want 0: %v`, got, *msgs)
	}
	if got := countMsg(*msgs, "route lookup failing"); got != 1 {
		t.Errorf(`"route lookup failing" logged %d times, want 1: %v`, got, *msgs)
	}

	r.errAll = nil
	tr.Tick(t.Context())
	if got := countMsg(*msgs, "route lookup recovered"); got != 1 {
		t.Errorf(`"route lookup recovered" logged %d times after a real answer, want 1: %v`, got, *msgs)
	}
}

func TestTick_RouteFailureLogsOnce(t *testing.T) {
	h, msgs := newRecordingHandler()
	ac := airborne("a", "ERR1", 51.48, -0.45)
	p := &fakeProvider{acs: []provider.Aircraft{ac}}
	r := &fakeRoutes{errFor: map[string]bool{"ERR1": true}}
	ck := &clock{t: time.Now()}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: r, Filter: filter.Filter{}, Now: ck.now,
		Logger: slog.New(h),
	})

	tr.Tick(t.Context())
	tr.Tick(t.Context())
	if got := countMsg(*msgs, "route lookup failing"); got != 1 {
		t.Errorf(`"route lookup failing" logged %d times, want 1: %v`, got, *msgs)
	}

	r.errFor = nil
	tr.Tick(t.Context())
	if got := countMsg(*msgs, "route lookup recovered"); got != 1 {
		t.Errorf(`"route lookup recovered" logged %d times, want 1: %v`, got, *msgs)
	}
}

// ctxErrProvider returns ctx.Err() to simulate a fetch that was cut short
// by shutdown rather than a genuine upstream failure.
type ctxErrProvider struct{}

func (ctxErrProvider) Fetch(ctx context.Context, lat, lon, r float64) ([]provider.Aircraft, error) {
	return nil, ctx.Err()
}

func TestTick_CancelledContextIsNotAFailure(t *testing.T) {
	h, msgs := newRecordingHandler()
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: ctxErrProvider{}, Routes: &fakeRoutes{}, Filter: filter.Filter{}, Now: ck.now,
		Logger: slog.New(h),
	})
	_, before := tr.Current()
	intervalBefore := tr.Interval()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tr.Tick(ctx)

	if tr.Interval() != intervalBefore {
		t.Errorf("interval changed on cancelled-context tick: %v, want %v", tr.Interval(), intervalBefore)
	}
	if _, at := tr.Current(); !at.Equal(before) {
		t.Errorf("last success changed on cancelled-context tick: %v, want %v", at, before)
	}
	if got := countMsg(*msgs, "upstream fetch failed"); got != 0 {
		t.Errorf(`"upstream fetch failed" logged %d times, want 0: %v`, got, *msgs)
	}
}

func TestTick_FailureKeepsSnapshotAndBacksOff(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("bbb", "NEAR1", 51.48, -0.45)}}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(p, &fakeRoutes{}, filter.Filter{}, ck)

	tr.Tick(t.Context())
	first := ck.t

	p.err = errors.New("upstream down")
	for i := 0; i < 3; i++ {
		ck.t = ck.t.Add(5 * time.Second)
		tr.Tick(t.Context())
		if i < 2 && tr.Interval() != 5*time.Second {
			t.Errorf("after %d failures interval = %v, want 5s", i+1, tr.Interval())
		}
	}
	if tr.Interval() != 30*time.Second {
		t.Errorf("after 3 failures interval = %v, want 30s", tr.Interval())
	}
	s, at := tr.Current()
	if !s.Found || s.Hex != "bbb" {
		t.Errorf("snapshot must survive failures: %+v", s)
	}
	if !at.Equal(first) {
		t.Errorf("last success must not advance on failure: %v vs %v", at, first)
	}

	p.err = nil
	ck.t = ck.t.Add(30 * time.Second)
	tr.Tick(t.Context())
	if tr.Interval() != 5*time.Second {
		t.Errorf("recovery must restore interval, got %v", tr.Interval())
	}
	if _, at := tr.Current(); !at.Equal(ck.t) {
		t.Error("recovery must advance last success")
	}
}

func TestTick_BackoffNeverShortensLongPoll(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("bbb", "NEAR1", 51.48, -0.45)}}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 60 * time.Second,
		Provider: p, Routes: &fakeRoutes{}, Filter: filter.Filter{}, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	tr.Tick(t.Context())

	p.err = errors.New("upstream down")
	for i := 0; i < 3; i++ {
		ck.t = ck.t.Add(time.Second)
		tr.Tick(t.Context())
	}
	if tr.Interval() != 60*time.Second {
		t.Errorf("after 3 failures with a 60s poll, interval = %v, want 60s", tr.Interval())
	}
}

func TestTick_RateLimitBacksOffImmediately(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("bbb", "NEAR1", 51.48, -0.45)}}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(p, &fakeRoutes{}, filter.Filter{}, ck)

	tr.Tick(t.Context())
	if tr.Interval() != 5*time.Second {
		t.Fatalf("interval before failure = %v, want 5s", tr.Interval())
	}

	p.err = provider.ErrRateLimited
	tr.Tick(t.Context())
	if tr.Interval() != 30*time.Second {
		t.Errorf("interval after one 429 = %v, want 30s (immediate backoff)", tr.Interval())
	}

	p.err = nil
	tr.Tick(t.Context())
	if tr.Interval() != 5*time.Second {
		t.Errorf("interval after recovery = %v, want 5s", tr.Interval())
	}
}

func TestCurrent_BeforeFirstTick(t *testing.T) {
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(&fakeProvider{}, &fakeRoutes{}, filter.Filter{}, ck)
	s, at := tr.Current()
	if s.Found {
		t.Error("nothing found before first tick")
	}
	if !at.Equal(ck.t) {
		t.Errorf("age reference before first tick should be start time, got %v", at)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	ck := &clock{t: time.Now()}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 2 * time.Second,
		Provider: &fakeProvider{}, Routes: &fakeRoutes{}, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

type fakeSettings struct {
	mu     sync.Mutex
	f      filter.Filter
	radius float64
	delay  bool
	show   bool
	paid   bool
	lat    float64
	lon    float64
}

// Home returns the configured centre; zero means "use the tracker's
// Options", which keeps the older tests on their Heathrow-ish default.
func (s *fakeSettings) Home() (float64, float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lat, s.lon, s.lat != 0 || s.lon != 0
}

func (s *fakeSettings) ShowDelay() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.show || s.delay
}

func (s *fakeSettings) Get() (filter.Filter, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f, s.radius
}

func (s *fakeSettings) set(f filter.Filter, r float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f, s.radius = f, r
}

func (s *fakeSettings) PaidRoutesWanted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paid
}

func (s *fakeSettings) DelayWanted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delay
}

type fakeDelay struct {
	mu      sync.Mutex
	calls   []string
	queries []flightstatus.Query
	st      flightstatus.Status
	ok      bool
	err     error
	// seq, when set, cycles through these errors one per call (repeating
	// the last once exhausted) instead of always returning err; it lets a
	// test simulate a real failure followed by cooldown/budget responses.
	seq []error
}

func (d *fakeDelay) Status(ctx context.Context, q flightstatus.Query) (flightstatus.Status, bool, error) {
	cs := q.Callsign
	d.queries = append(d.queries, q)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, cs)
	err := d.err
	if d.seq != nil {
		i := len(d.calls) - 1
		if i >= len(d.seq) {
			i = len(d.seq) - 1
		}
		err = d.seq[i]
	}
	return d.st, d.ok, err
}

func delayTracker(p provider.Provider, d flightstatus.Lookup, want bool) (*Tracker, *fakeSettings) {
	src := &fakeSettings{f: filter.Filter{}, radius: 10, delay: want, show: want}
	ck := &clock{t: time.Now()}
	return New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: &fakeRoutes{}, Settings: src, Delay: d, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), src
}

func TestTick_AttachesDelayForNearestOnly(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{
		airborne("far", "FAR1", 51.50, -0.45),
		airborne("near", "NEAR1", 51.475, -0.45),
	}}
	d := &fakeDelay{st: flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 12, Basis: "arrival"}, ok: true}
	tr, _ := delayTracker(p, d, true)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Delay == nil || s.Delay.DelayMin != 12 {
		t.Fatalf("delay not attached: %+v", s.Delay)
	}
	if len(d.calls) != 1 || d.calls[0] != "NEAR1" {
		t.Errorf("lookup must be for the nearest only: %v", d.calls)
	}
	list, _ := tr.List()
	if len(list) != 2 || list[0].Delay == nil || list[0].Delay.DelayMin != 12 || list[1].Delay != nil {
		t.Errorf("delay must ride on the list head only: %+v", list)
	}
}

func TestTick_NoDelayWhenOffOrNil(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}}
	d := &fakeDelay{st: flightstatus.Status{Kind: flightstatus.KindOnTime}, ok: true}
	tr, _ := delayTracker(p, d, false)
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Delay != nil || len(d.calls) != 0 {
		t.Errorf("DelayWanted=false must skip lookups: %+v calls=%v", s.Delay, d.calls)
	}
	tr2, _ := delayTracker(p, nil, true)
	tr2.Tick(t.Context())
	if s, _ := tr2.Current(); s.Delay != nil {
		t.Errorf("nil lookup must skip: %+v", s.Delay)
	}
}

func TestTick_DelayLookupErrorDoesNotFailTick(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}}
	d := &fakeDelay{err: errors.New("aeroapi down")}
	// Captured before delayTracker constructs the fake clock (which is
	// fixed at construction time), so a correctly-updated lastSuccess
	// cannot spuriously read as "before" this mark.
	before := time.Now()
	tr, _ := delayTracker(p, d, true)
	tr.Tick(t.Context())
	s, at := tr.Current()
	if !s.Found || s.Delay != nil {
		t.Errorf("aircraft must still show without delay: %+v", s)
	}
	if at.Before(before) {
		t.Error("tick must count as a success")
	}
	if tr.Interval() != 5*time.Second {
		t.Error("a delay error must not trigger backoff")
	}
}

func TestTick_UnknownDelayIsOmitted(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}}
	d := &fakeDelay{ok: false}
	tr, _ := delayTracker(p, d, true)
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Delay != nil {
		t.Errorf("unknown must be omitted: %+v", s.Delay)
	}
}

// TestTick_DelayCooldownDoesNotFlapLogs: a genuine failure logs "delay
// lookup failing" once; the cache's ErrCooldown responses that follow are
// a consequence of that failure, not fresh information, and must not log
// "recovered" (the delay status still isn't there).
func TestTick_DelayCooldownDoesNotFlapLogs(t *testing.T) {
	h, msgs := newRecordingHandler()
	p := &fakeProvider{acs: []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}}
	d := &fakeDelay{seq: []error{errors.New("aeroapi down"), flightstatus.ErrCooldown, flightstatus.ErrCooldown}}
	src := &fakeSettings{f: filter.Filter{}, radius: 10, delay: true}
	ck := &clock{t: time.Now()}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: &fakeRoutes{}, Settings: src, Delay: d, Now: ck.now,
		Logger: slog.New(h),
	})

	tr.Tick(t.Context())
	tr.Tick(t.Context())
	tr.Tick(t.Context())

	if got := countMsg(*msgs, "AeroAPI lookup failing"); got != 1 {
		t.Errorf(`"AeroAPI lookup failing" logged %d times, want 1: %v`, got, *msgs)
	}
	if got := countMsg(*msgs, "AeroAPI lookup recovered"); got != 0 {
		t.Errorf(`"AeroAPI lookup recovered" logged %d times, want 0: %v`, got, *msgs)
	}
}

func TestTick_NoCallsignSkipsDelay(t *testing.T) {
	ac := airborne("near", "", 51.475, -0.45)
	p := &fakeProvider{acs: []provider.Aircraft{ac}}
	d := &fakeDelay{ok: true}
	tr, _ := delayTracker(p, d, true)
	tr.Tick(t.Context())
	if len(d.calls) != 0 {
		t.Errorf("empty callsign must not be looked up: %v", d.calls)
	}
}

type radiusRecorder struct {
	acs    []provider.Aircraft
	radius float64
}

func (r *radiusRecorder) Fetch(ctx context.Context, lat, lon, radiusKm float64) ([]provider.Aircraft, error) {
	r.radius = radiusKm
	return r.acs, nil
}

func TestTick_UsesLiveSettings(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{
		"DEP1": {OriginIATA: "AGP", DestIATA: "LHR"},
		"ARR1": {OriginIATA: "LHR", DestIATA: "AGP"},
		"DEP2": {OriginIATA: "AGP", DestIATA: "MAD"},
	}}
	p := &radiusRecorder{acs: []provider.Aircraft{
		airborne("a", "ARR1", 51.475, -0.45),   // nearest, ~1 km
		airborne("d", "DEP1", 51.49, -0.45),    // ~2 km
		airborne("far", "DEP2", 51.605, -0.45), // ~15 km north; excluded by live radius 10, but inside Options.RadiusKm 99
	}}
	fromAGP, _ := filter.Parse("AGP", "", "")
	src := &fakeSettings{f: filter.Filter{}, radius: 10}
	ck := &clock{t: time.Now()}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 99, Poll: 5 * time.Second,
		Provider: p, Routes: r, Filter: fromAGP, Settings: src, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Hex != "a" || s.Nearby != 2 || p.radius != 10 {
		t.Fatalf("tick 1 must use the live (inactive, 10 km) settings, not Options: %+v radius=%v", s, p.radius)
	}
	if s.RadiusKm != 10 {
		t.Errorf("tick 1 snapshot RadiusKm = %v, want 10", s.RadiusKm)
	}

	src.set(fromAGP, 25)
	tr.Tick(t.Context())
	s, _ = tr.Current()
	if s.Hex != "d" || s.Nearby != 2 || p.radius != 25 {
		t.Fatalf("tick 2 must see the changed settings: %+v radius=%v", s, p.radius)
	}
	if s.RadiusKm != 25 {
		t.Errorf("tick 2 snapshot RadiusKm = %v, want 25", s.RadiusKm)
	}
}

// TestTick_ConcurrentSettingsChange exercises Tick against a real
// settings.Current (not a fake) under concurrent Set calls, so -race can
// catch a data race between the tracker's read side and settings' write
// side that a fake SettingsSource wouldn't exercise. No assertions beyond
// completion: the -race run itself is the check.
func TestTick_ConcurrentSettingsChange(t *testing.T) {
	c, err := settings.NewCurrent(settings.Settings{RadiusKm: 10}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{acs: []provider.Aircraft{airborne("bbb", "NEAR1", 51.48, -0.45)}}
	ck := &clock{t: time.Now()}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 99, Poll: 5 * time.Second,
		Provider: p, Routes: &fakeRoutes{}, Settings: c, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			r := 10.0
			if i%2 == 1 {
				r = 25.0
			}
			_, _ = c.Set(settings.Settings{RadiusKm: r})
		}
	}()

	for i := 0; i < 50; i++ {
		tr.Tick(t.Context())
	}
	<-done
}

func TestTick_ListIsSortedAndIndexed(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{}}
	ck := &clock{t: time.Now()}
	acs := []provider.Aircraft{
		airborne("c", "FAR1", 51.53, -0.45),  // ~6.7 km
		airborne("a", "NEAR1", 51.48, -0.45), // ~1.1 km
		airborne("b", "MID1", 51.50, -0.45),  // ~3.3 km
	}
	tr := newTracker(&fakeProvider{acs: acs}, r, filter.Filter{}, ck)
	tr.Tick(t.Context())
	list, at := tr.List()
	if at.IsZero() || len(list) != 3 {
		t.Fatalf("len=%d at=%v", len(list), at)
	}
	for i, want := range []string{"a", "b", "c"} {
		if list[i].Hex != want || list[i].Index != i || list[i].Nearby != 3 || list[i].RadiusKm != 10 || !list[i].Found {
			t.Errorf("list[%d] = %+v", i, list[i])
		}
	}
	cur, _ := tr.Current()
	if cur.Hex != "a" || cur.Index != 0 {
		t.Errorf("current = %+v", cur)
	}
	list[0].Hex = "mutated"
	again, _ := tr.List()
	if again[0].Hex != "a" {
		t.Error("List must return a copy")
	}
}

func TestList_EmptyBeforeTraffic(t *testing.T) {
	tr := newTracker(&fakeProvider{}, &fakeRoutes{}, filter.Filter{}, &clock{t: time.Now()})
	if list, _ := tr.List(); len(list) != 0 {
		t.Errorf("got %v", list)
	}
	tr.Tick(t.Context())
	if list, _ := tr.List(); len(list) != 0 {
		t.Errorf("after empty tick: %v", list)
	}
}

func TestTick_BoardDelayForEveryListedAircraft(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{
		airborne("far", "FAR1", 51.50, -0.45),
		airborne("near", "NEAR1", 51.475, -0.45),
	}}
	board := &fakeDelay{st: flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 3, Basis: "arrival"}, ok: true}
	paid := &fakeDelay{st: flightstatus.Status{Kind: flightstatus.KindOnTime}, ok: true}
	tr, _ := delayTracker(p, paid, true)
	tr.o.BoardDelay = board
	tr.Tick(t.Context())
	list, _ := tr.List()
	if len(list) != 2 || list[0].Delay == nil || list[1].Delay == nil || list[1].Delay.DelayMin != 3 {
		t.Fatalf("board status must ride on every entry: %+v", list)
	}
	if len(paid.calls) != 0 {
		t.Errorf("the board answered the nearest, so the paid lookup must not run: %v", paid.calls)
	}
	board.ok = false // board knows nothing → nearest falls back to the paid chain, others stay nil
	tr.Tick(t.Context())
	list, _ = tr.List()
	if list[0].Delay == nil || list[0].Delay.Kind != flightstatus.KindOnTime || list[1].Delay != nil || len(paid.calls) != 1 {
		t.Errorf("fallback: %+v paid=%v", list, paid.calls)
	}
}

func TestTick_BoardWorksWhenThePaidCapIsZero(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}}
	board := &fakeDelay{st: flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 3, Basis: "arrival"}, ok: true}
	paid := &fakeDelay{st: flightstatus.Status{Kind: flightstatus.KindOnTime}, ok: true}
	tr, src := delayTracker(p, paid, true)
	src.delay = false // DelayWanted() false: cap 0 / paid lookups off
	src.show = true   // but the user still wants delay shown
	tr.o.BoardDelay = board
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Delay == nil || s.Delay.DelayMin != 3 || len(paid.calls) != 0 {
		t.Errorf("board must run on show_delay alone: %+v paid=%v", s.Delay, paid.calls)
	}
}

func TestTick_QueryCarriesTheFirstSighting(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}}
	board := &fakeDelay{ok: false}
	tr, _ := delayTracker(p, &fakeDelay{ok: false}, true)
	tr.o.BoardDelay = board
	ck := &clock{t: time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)}
	tr.o.Now = ck.now
	tr.Tick(t.Context())
	ck.t = ck.t.Add(time.Minute)
	tr.Tick(t.Context())
	if len(board.queries) != 2 || !board.queries[1].Seen.Equal(time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("the second tick must still carry the first sighting: %+v", board.queries)
	}
	// A one-tick reception flicker must not reset the sighting.
	p.acs = nil
	ck.t = ck.t.Add(5 * time.Second)
	tr.Tick(t.Context())
	p.acs = []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}
	ck.t = ck.t.Add(5 * time.Second)
	tr.Tick(t.Context())
	if last := board.queries[len(board.queries)-1]; !last.Seen.Equal(time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("a brief gap must keep the first sighting: %+v", last)
	}
	// Gone for longer than the grace period → forgotten; back → fresh.
	p.acs = nil
	tr.Tick(t.Context())
	p.acs = []provider.Aircraft{airborne("near", "NEAR1", 51.475, -0.45)}
	ck.t = ck.t.Add(10 * time.Minute)
	tr.Tick(t.Context())
	if last := board.queries[len(board.queries)-1]; !last.Seen.Equal(ck.t) {
		t.Errorf("a returning aircraft gets a new sighting: %+v", last)
	}
}

func TestTick_HomeComesFromSettings(t *testing.T) {
	// The aircraft is near Madrid, far from the configured Heathrow-ish
	// Options.Lat/Lon; the settings source moves home to Madrid.
	p := &fakeProvider{acs: []provider.Aircraft{airborne("mad", "MAD1", 40.495, -3.565)}}
	src := &fakeSettings{f: filter.Filter{}, radius: 10, lat: 40.49, lon: -3.57}
	ck := &clock{t: time.Now()}
	tr := New(Options{Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: &fakeRoutes{}, Settings: src, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if !s.Found || s.DistKm > 2 {
		t.Errorf("home must follow the settings: %+v", s)
	}
	if p.lat < 40.4 || p.lat > 40.6 {
		t.Errorf("the feed must be asked around the settings home, got %v,%v", p.lat, p.lon)
	}
}

type opCall struct {
	callsign, hex string
	haveAirline   bool
}

type fakeOperators struct {
	res   standing.Airline
	ok    bool
	err   error
	calls []opCall
}

func (f *fakeOperators) Operator(ctx context.Context, callsign, hex string, haveAirline bool) (standing.Airline, bool, error) {
	f.calls = append(f.calls, opCall{callsign, hex, haveAirline})
	return f.res, f.ok, f.err
}

func newOpTracker(p provider.Provider, r routes.Lookup, ops OperatorLookup) *Tracker {
	return New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: r, Operators: ops, Now: time.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestTick_OperatorFromPrefixOverridesRouteAirline(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("aaa", "SCR137", 51.48, -0.45)}}
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"SCR137": {Airline: "Si-Chang Flying Service"}}}
	ops := &fakeOperators{res: standing.Airline{Name: "Silver Cloud Air"}, ok: true}
	tr := newOpTracker(p, r, ops)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Airline != "Silver Cloud Air" || s.AirlineIATA != "" {
		t.Errorf("airline = %q/%q", s.Airline, s.AirlineIATA)
	}
}

// The logo must belong to the airline whose name is shown: VRS's code wins,
// and the route's code survives only when VRS has none.
func TestTick_OperatorIATAPrecedence(t *testing.T) {
	for _, c := range []struct {
		name     string
		op       standing.Airline
		wantName string
		wantIATA string
	}{
		{"vrs code replaces route code", standing.Airline{Name: "Jet2.com", IATA: "XX"}, "Jet2.com", "XX"},
		{"route code kept when vrs has none", standing.Airline{Name: "Silver Cloud Air"}, "Silver Cloud Air", "LS"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &fakeProvider{acs: []provider.Aircraft{airborne("aaa", "EXS1", 51.48, -0.45)}}
			r := &fakeRoutes{byCallsign: map[string]routes.Route{"EXS1": {Airline: "Jet2", AirlineIATA: "LS"}}}
			tr := newOpTracker(p, r, &fakeOperators{res: c.op, ok: true})
			tr.Tick(t.Context())
			s, _ := tr.Current()
			if s.Airline != c.wantName || s.AirlineIATA != c.wantIATA {
				t.Errorf("airline = %q/%q, want %q/%q", s.Airline, s.AirlineIATA, c.wantName, c.wantIATA)
			}
		})
	}
}

func TestTick_OperatorForNoCallsign(t *testing.T) {
	ac := airborne("3451d5", "", 51.48, -0.45)
	ac.Reg = "EC-MKO"
	p := &fakeProvider{acs: []provider.Aircraft{ac}}
	ops := &fakeOperators{res: standing.Airline{Name: "Vueling Airlines", IATA: "VY"}, ok: true}
	tr := newOpTracker(p, &fakeRoutes{}, ops)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Callsign != "EC-MKO" || s.Airline != "Vueling Airlines" || s.AirlineIATA != "VY" {
		t.Errorf("snapshot = %+v", s)
	}
	if s.Origin != "" || s.Dest != "" {
		t.Errorf("route fields must stay empty: %+v", s)
	}
	if len(ops.calls) != 1 || ops.calls[0].haveAirline {
		t.Errorf("calls = %+v", ops.calls)
	}
}

func TestTick_HaveAirlinePassedThrough(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("aaa", "BAW1", 51.48, -0.45)}}
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"BAW1": {Airline: "British Airways"}}}
	ops := &fakeOperators{}
	tr := newOpTracker(p, r, ops)
	tr.Tick(t.Context())
	if len(ops.calls) != 1 || !ops.calls[0].haveAirline {
		t.Errorf("calls = %+v", ops.calls)
	}
}

func TestTick_OperatorErrorIgnored(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("aaa", "BAW1", 51.48, -0.45)}}
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"BAW1": {Airline: "British Airways"}}}
	ops := &fakeOperators{err: errors.New("boom")}
	tr := newOpTracker(p, r, ops)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if !s.Found || s.Airline != "British Airways" {
		t.Errorf("snapshot = %+v", s)
	}
	if tr.routeFailing {
		t.Error("operator errors must not mark route lookups as failing")
	}
}

func TestTick_TypeName(t *testing.T) {
	a := airborne("aaa", "X1", 51.48, -0.45)
	a.Type = "C56X"
	b := airborne("bbb", "X2", 51.49, -0.45)
	b.Type = "ZZZZ"
	tr := newOpTracker(&fakeProvider{acs: []provider.Aircraft{a, b}}, &fakeRoutes{}, nil)
	tr.Tick(t.Context())
	l, _ := tr.List()
	if l[0].TypeName != "Citation XLS" || l[1].TypeName != "" {
		t.Errorf("type names = %q, %q", l[0].TypeName, l[1].TypeName)
	}
}

func TestTick_NilOperators(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("aaa", "BAW1", 51.48, -0.45)}}
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"BAW1": {Airline: "British Airways"}}}
	tr := newOpTracker(p, r, nil)
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Airline != "British Airways" {
		t.Errorf("airline = %q", s.Airline)
	}
}

// blockingOperators waits for its context, like a stalled adsbdb.
type blockingOperators struct{ calls int }

func (b *blockingOperators) Operator(ctx context.Context, _, _ string, _ bool) (standing.Airline, bool, error) {
	b.calls++
	<-ctx.Done()
	return standing.Airline{}, false, ctx.Err()
}

func TestTick_OperatorLookupsShareOneBudget(t *testing.T) {
	var acs []provider.Aircraft
	for i, hex := range []string{"3451d1", "3451d2", "3451d3", "3451d4", "3451d5"} {
		acs = append(acs, airborne(hex, "", 51.48+float64(i)*0.001, -0.45))
	}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		RouteTimeout: 50 * time.Millisecond,
		Provider:     &fakeProvider{acs: acs}, Routes: &fakeRoutes{}, Operators: &blockingOperators{}, Now: time.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	start := time.Now()
	tr.Tick(t.Context())
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("Tick took %v, want < 150ms", d)
	}
	if s, _ := tr.Current(); !s.Found || s.Nearby != 5 {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestTick_FilteredAircraftMakeNoOperatorCall(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("aaa", "BAW1", 51.48, -0.45)}}
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"BAW1": {OriginIATA: "LHR", DestIATA: "BCN"}}}
	ops := &fakeOperators{}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: p, Routes: r, Operators: ops, Now: time.Now,
		Filter: mustFilter(t, "", "", "CDG"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Found {
		t.Fatalf("aircraft should be filtered out: %+v", s)
	}
	if len(ops.calls) != 0 {
		t.Errorf("operator calls = %+v, want none", ops.calls)
	}
}

func mustFilter(t *testing.T, o, d, a string) filter.Filter {
	t.Helper()
	f, err := filter.Parse(o, d, a)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// fakeFlights stands in for AeroAPI behind the real cache, so the tests
// count upstream calls the way the paid budget does.
type fakeFlights struct {
	mu      sync.Mutex
	queries []flightstatus.Query
	fl      flightstatus.Flight
	ok      bool
	err     error
}

func (f *fakeFlights) Flight(ctx context.Context, q flightstatus.Query) (flightstatus.Flight, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	return f.fl, f.ok, f.err
}

func (f *fakeFlights) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

type fakeBoardRoute struct {
	r     flightstatus.FlightRoute
	ok    bool
	calls []string
}

func (b *fakeBoardRoute) Route(q flightstatus.Query, now time.Time) (flightstatus.FlightRoute, bool) {
	b.calls = append(b.calls, q.Callsign)
	return b.r, b.ok
}

var agpLGW = flightstatus.FlightRoute{
	OriginIATA: "AGP", OriginICAO: "LEMG", OriginCity: "Malaga",
	DestIATA: "LGW", DestICAO: "EGKK", DestCity: "London",
	Operator: "EZY", OperatorIATA: "U2",
}

type paidRig struct {
	tr    *Tracker
	src   *fakeSettings
	ck    *clock
	inner *fakeFlights
	rts   *fakeRoutes
}

// newPaidRig wires the same cache as both Delay and Flights, as main does,
// so a route call and a delay call for one ident share an upstream call.
func newPaidRig(acs []provider.Aircraft, delay, paid bool) *paidRig {
	ck := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	inner := &fakeFlights{}
	cache := flightstatus.NewCache(inner, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	src := &fakeSettings{radius: 10, delay: delay, show: delay, paid: paid}
	rts := &fakeRoutes{byCallsign: map[string]routes.Route{}}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: &fakeProvider{acs: acs}, Routes: rts, Settings: src,
		Delay: cache, Flights: cache, Now: ck.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &paidRig{tr: tr, src: src, ck: ck, inner: inner, rts: rts}
}

func (r *paidRig) tick(t *testing.T) Snapshot {
	t.Helper()
	r.tr.Tick(t.Context())
	s, _ := r.tr.Current()
	return s
}

func TestPaidRoute_FreeRouteMeansNoPaidCall(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "BAW466", 51.48, -0.45)}, false, true)
	rig.rts.byCallsign["BAW466"] = routes.Route{OriginIATA: "LHR", DestIATA: "BCN"}
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	s := rig.tick(t)
	if rig.inner.calls() != 0 || s.Origin != "LHR" || s.Dest != "BCN" {
		t.Errorf("free route must win with no paid call: calls=%d %+v", rig.inner.calls(), s)
	}
}

func TestPaidRoute_BoardBeforePaying(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}, false, true)
	brd := &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "AGP", OriginICAO: "LEMG", DestIATA: "LGW", DestICAO: "EGKK"}, ok: true}
	rig.tr.o.BoardRoute = brd
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	s := rig.tick(t)
	if rig.inner.calls() != 0 || s.Origin != "AGP" || s.Dest != "LGW" {
		t.Fatalf("board route must be used before paying: calls=%d %+v", rig.inner.calls(), s)
	}
	if len(brd.calls) != 1 || brd.calls[0] != "EZY12" {
		t.Errorf("board asked with %v", brd.calls)
	}
	// The board row ageing out mid-pass must not cost a paid call.
	brd.ok = false
	rig.ck.t = rig.ck.t.Add(time.Minute)
	if s := rig.tick(t); rig.inner.calls() != 0 || s.Origin != "AGP" {
		t.Errorf("remembered board route: calls=%d %+v", rig.inner.calls(), s)
	}
}

func TestPaidRoute_FillsRouteNamesAndAirline(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{
		airborne("near", "EZY12", 51.475, -0.45),
		airborne("far", "FAR1", 51.50, -0.45),
	}, false, true)
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	s := rig.tick(t)
	if s.Origin != "AGP" || s.Dest != "LGW" || s.OriginName != "Malaga" || s.DestName != "London" ||
		s.Airline != "EZY" || s.AirlineIATA != "U2" {
		t.Fatalf("paid route not applied: %+v", s)
	}
	if s.Index != 0 || s.Nearby != 2 || s.Hex != "near" {
		t.Errorf("snapshot identity lost: %+v", s)
	}
	list, _ := rig.tr.List()
	if len(list) != 2 || list[0].Origin != "AGP" || list[0].Index != 0 || list[1].Origin != "" || list[1].Index != 1 {
		t.Errorf("list head must carry the route, others untouched: %+v", list)
	}
	if rig.inner.calls() != 1 || rig.inner.queries[0].Callsign != "EZY12" {
		t.Errorf("only the nearest is looked up: %+v", rig.inner.queries)
	}
}

func TestPaidRoute_KeepsAirlineFromFreeSources(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}, false, true)
	rig.tr.o.Operators = &fakeOperators{res: standing.Airline{Name: "easyJet Europe", IATA: "EC"}, ok: true}
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	s := rig.tick(t)
	if s.Origin != "AGP" || s.Airline != "easyJet Europe" || s.AirlineIATA != "EC" {
		t.Errorf("free airline must be kept: %+v", s)
	}
}

func TestPaidRoute_UnknownRememberedForThePass(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "PRIV1", 51.48, -0.45)}, false, true)
	rig.inner.ok = false
	for i := 0; i < 20; i++ { // 20 minutes overhead: past the 15-min cache entry
		if s := rig.tick(t); s.Origin != "" {
			t.Fatalf("unknown must stay empty: %+v", s)
		}
		rig.ck.t = rig.ck.t.Add(time.Minute)
	}
	if rig.inner.calls() != 1 {
		t.Errorf("one route-only call per pass, got %d", rig.inner.calls())
	}
}

func TestPaidRoute_FoundRouteKeptForThePassThenNewPassMayCall(t *testing.T) {
	ac := airborne("a", "EZY12", 51.48, -0.45)
	rig := newPaidRig([]provider.Aircraft{ac}, false, true)
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	rig.tick(t)
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{}, false // AeroAPI would now say nothing
	for i := 0; i < 20; i++ {
		rig.ck.t = rig.ck.t.Add(time.Minute)
		if s := rig.tick(t); s.Origin != "AGP" {
			t.Fatalf("route must stay for the pass (minute %d): %+v", i+1, s)
		}
	}
	if rig.inner.calls() != 1 {
		t.Fatalf("calls within the pass = %d, want 1", rig.inner.calls())
	}
	// Gone past the sighting grace: the pass, and its memory, end.
	rig.tr.o.Provider = &fakeProvider{}
	rig.tick(t)
	rig.ck.t = rig.ck.t.Add(10 * time.Minute)
	rig.tr.o.Provider = &fakeProvider{acs: []provider.Aircraft{ac}}
	rig.tick(t)
	if rig.inner.calls() != 2 {
		t.Errorf("a new pass may call again: calls=%d", rig.inner.calls())
	}
}

func TestPaidRoute_LightAircraftSkipped(t *testing.T) {
	a1 := airborne("a", "GLIDE1", 51.48, -0.45)
	a1.Category = "A1"
	anon := airborne("b", "", 51.48, -0.45) // no category, no callsign: local GA
	for _, ac := range []provider.Aircraft{a1, anon} {
		rig := newPaidRig([]provider.Aircraft{ac}, false, true)
		rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
		if s := rig.tick(t); rig.inner.calls() != 0 || s.Origin != "" {
			t.Errorf("%s: light aircraft must not be paid for: calls=%d %+v", ac.Hex, rig.inner.calls(), s)
		}
	}
}

func TestPaidRoute_RegistrationIdent(t *testing.T) {
	ac := airborne("a", "", 51.48, -0.45)
	ac.Reg, ac.Category = "EC-MKO", "A3"
	rig := newPaidRig([]provider.Aircraft{ac}, false, true)
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	s := rig.tick(t)
	if rig.inner.calls() != 1 || rig.inner.queries[0].Callsign != "ECMKO" || s.Origin != "AGP" {
		t.Errorf("registration ident: %+v %+v", rig.inner.queries, s)
	}
}

func TestPaidRoute_DelayAndRouteShareOneCall(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}, true, true)
	late := flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 20, Basis: "arrival"}
	rig.inner.fl = flightstatus.Flight{Route: agpLGW, HasRoute: true, Status: late, HasStatus: true}
	rig.inner.ok = true
	s := rig.tick(t)
	if rig.inner.calls() != 1 {
		t.Errorf("route and delay must share one AeroAPI call, got %d", rig.inner.calls())
	}
	if s.Origin != "AGP" || s.Delay == nil || s.Delay.DelayMin != 20 {
		t.Errorf("route and delay both shown: %+v delay=%+v", s, s.Delay)
	}
	if list, _ := rig.tr.List(); list[0].Delay == nil || list[0].Origin != "AGP" {
		t.Errorf("list head: %+v", list[0])
	}
}

func TestPaidRoute_OffKeepsDelayAsToday(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}, true, false)
	rig.inner.fl = flightstatus.Flight{Route: agpLGW, HasRoute: true,
		Status: flightstatus.Status{Kind: flightstatus.KindOnTime}, HasStatus: true}
	rig.inner.ok = true
	s := rig.tick(t)
	if s.Origin != "" || s.Dest != "" || s.Airline != "" {
		t.Errorf("paid_routes off: no AeroAPI route may be used: %+v", s)
	}
	if s.Delay == nil || s.Delay.Kind != flightstatus.KindOnTime || rig.inner.calls() != 1 {
		t.Errorf("delay as today: %+v calls=%d", s.Delay, rig.inner.calls())
	}
}

func TestPaidRoute_OnWithShowDelayOff(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}, false, true)
	rig.inner.fl = flightstatus.Flight{Route: agpLGW, HasRoute: true,
		Status: flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 30}, HasStatus: true}
	rig.inner.ok = true
	s := rig.tick(t)
	if s.Origin != "AGP" || s.Delay != nil {
		t.Errorf("route shown, delay hidden: %+v delay=%+v", s, s.Delay)
	}
}

func TestPaidRoute_DelayKeyedByIdentSharesTheCall(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "ezy12 ", 51.48, -0.45)}, false, true)
	rig.inner.fl = flightstatus.Flight{Route: agpLGW, HasRoute: true,
		Status: flightstatus.Status{Kind: flightstatus.KindOnTime}, HasStatus: true}
	rig.inner.ok = true
	rig.tick(t)
	rig.src.delay, rig.src.show = true, true
	rig.ck.t = rig.ck.t.Add(5 * time.Second)
	s := rig.tick(t)
	if rig.inner.calls() != 1 || s.Delay == nil {
		t.Errorf("delay must hit the route call's cache entry: calls=%d delay=%+v", rig.inner.calls(), s.Delay)
	}
}

// A refusal is not cached: asking the budget again is free, and a raised
// cap must take effect on the very next tick rather than after a cooldown.
func TestPaidRoute_BudgetRefusalIsAskedAgainEachTick(t *testing.T) {
	h, msgs := newRecordingHandler()
	ck := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	inner := &fakeFlights{fl: flightstatus.Flight{Route: agpLGW, HasRoute: true}, ok: true}
	limit := 1
	budget := flightstatus.NewBudget(func() int { return limit }, ck.now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	budget.Allow() // the day's only lookup is already spent
	refusals := &countingFlights{inner: flightstatus.NewBudgeted(inner, budget)}
	cache := flightstatus.NewCache(refusals, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	src := &fakeSettings{radius: 10, delay: true, paid: true}
	tr := New(Options{
		Lat: homeLat, Lon: homeLon, RadiusKm: 10, Poll: 5 * time.Second,
		Provider: &fakeProvider{acs: []provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}},
		Routes:   &fakeRoutes{}, Settings: src, Delay: cache, Flights: cache, Now: ck.now,
		Logger: slog.New(h),
	})
	for i := 0; i < 3; i++ {
		tr.Tick(t.Context())
		if s, _ := tr.Current(); s.Origin != "" || s.Delay != nil {
			t.Fatalf("refused budget must leave the route empty: %+v", s)
		}
		ck.t = ck.t.Add(5 * time.Second)
	}
	if refusals.n != 3 || inner.calls() != 0 {
		t.Fatalf("budget asked %d times, upstream %d; want 3 and 0", refusals.n, inner.calls())
	}
	limit = 2 // raised in Settings
	tr.Tick(t.Context())
	if inner.calls() != 1 {
		t.Errorf("a raised cap must reach upstream on the next tick: upstream %d", inner.calls())
	}
	for _, m := range *msgs {
		if m == "AeroAPI lookup failing" {
			t.Errorf("a budget refusal is not a failure: %v", *msgs)
		}
	}
}

type countingFlights struct {
	inner flightstatus.FlightLookup
	n     int
}

func (c *countingFlights) Flight(ctx context.Context, q flightstatus.Query) (flightstatus.Flight, bool, error) {
	c.n++
	return c.inner.Flight(ctx, q)
}

func TestPaidRoute_FiltersUseTheFreeRouteOnly(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "EZY12", 51.48, -0.45)}, false, true)
	rig.src.f = mustFilter(t, "AGP", "", "")
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	if s := rig.tick(t); s.Found || rig.inner.calls() != 0 {
		t.Errorf("a paid route must not let a filtered aircraft through: %+v calls=%d", s, rig.inner.calls())
	}
}

func TestPaidRoute_RegistrationIdentKeepsDelayAcrossTicks(t *testing.T) {
	ac := airborne("a", "", 51.48, -0.45)
	ac.Reg, ac.Category = "EC-MKO", "A3"
	rig := newPaidRig([]provider.Aircraft{ac}, true, true)
	late := flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 15, Basis: "arrival"}
	rig.inner.fl = flightstatus.Flight{Route: agpLGW, HasRoute: true, Status: late, HasStatus: true}
	rig.inner.ok = true
	for i := 1; i <= 2; i++ {
		s := rig.tick(t)
		if s.Origin != "AGP" || s.Delay == nil || s.Delay.DelayMin != 15 {
			t.Fatalf("tick %d: route and delay must both show: %+v delay=%+v", i, s, s.Delay)
		}
		rig.ck.t = rig.ck.t.Add(5 * time.Second)
	}
	if rig.inner.calls() != 1 {
		t.Errorf("one upstream call for both ticks, got %d: %+v", rig.inner.calls(), rig.inner.queries)
	}
}

func TestPaidRoute_OffMeansNoCallForNoCallsign(t *testing.T) {
	ac := airborne("a", "", 51.48, -0.45)
	ac.Reg, ac.Category = "EC-MKO", "A3"
	rig := newPaidRig([]provider.Aircraft{ac}, true, false)
	rig.inner.fl = flightstatus.Flight{Route: agpLGW, HasRoute: true,
		Status: flightstatus.Status{Kind: flightstatus.KindOnTime}, HasStatus: true}
	rig.inner.ok = true
	rig.tick(t)
	rig.tick(t)
	if rig.inner.calls() != 0 {
		t.Errorf("paid_routes off: a registration ident must not be looked up: %+v", rig.inner.queries)
	}
}

func TestPaidRoute_LightNoCallsignGetsNoDelayCall(t *testing.T) {
	ac := airborne("a", "", 51.48, -0.45) // no category, no callsign: local GA
	rig := newPaidRig([]provider.Aircraft{ac}, true, true)
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Status: flightstatus.Status{Kind: flightstatus.KindOnTime}, HasStatus: true}, true
	rig.tick(t)
	if rig.inner.calls() != 0 {
		t.Errorf("light aircraft must not be paid for via the delay path: %+v", rig.inner.queries)
	}
}

// agpTFS is a free route whose airports are nowhere near home (Heathrow), as
// a stale adsbdb route for a reused flight number is.
var agpTFS = routes.Route{Airline: "Wizz Air", AirlineIATA: "W6", OriginIATA: "AGP", DestIATA: "TFS",
	OriginName: "Malaga", DestName: "Tenerife",
	HasCoords: true, OriginLat: 36.67, OriginLon: -4.49, DestLat: 28.04, DestLon: -16.57}

func TestTick_ImplausibleFreeRouteIsDropped(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": agpTFS}}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, r, filter.Filter{}, &clock{t: time.Now()})
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Origin != "" || s.Dest != "" || s.OriginName != "" || s.DestName != "" {
		t.Errorf("route the aircraft cannot be flying must be dropped: %+v", s)
	}
	if s.Airline != "Wizz Air" || s.AirlineIATA != "W6" {
		t.Errorf("airline comes from the callsign and must stay: %+v", s)
	}
}

func TestPaidRoute_ImplausibleFreeRouteGoesToFlightAware(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, false, true)
	rig.rts.byCallsign["WZZ1"] = agpTFS
	rig.tr.o.BoardRoute = &fakeBoardRoute{}
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	s := rig.tick(t)
	if s.Origin != "AGP" || s.Dest != "LGW" || rig.inner.calls() != 1 {
		t.Errorf("an implausible route the board cannot replace goes to AeroAPI: calls=%d %+v", rig.inner.calls(), s)
	}
}

// lhrBUD is a free route that is plausible from home but stale: the board,
// which has today's times, says the flight goes elsewhere.
var lhrBUD = routes.Route{Airline: "Wizz Air", AirlineIATA: "W6", OriginIATA: "LHR", DestIATA: "BUD",
	OriginName: "London", DestName: "Budapest",
	HasCoords: true, OriginLat: 51.47, OriginLon: -0.46, DestLat: 47.43, DestLon: 19.26}

func TestTick_BoardRouteOverridesFreeRoute(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrBUD}}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, r, filter.Filter{}, &clock{t: time.Now()})
	tr.o.BoardRoute = &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", OriginICAO: "EGLL", DestIATA: "WAW", DestICAO: "EPWA"}, ok: true}
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Origin != "LHR" || s.Dest != "WAW" || s.DestName != "Warsaw" {
		t.Errorf("board route must win, named from the airport table, not the free route: %+v", s)
	}
	if s.Airline != "Wizz Air" || s.AirlineIATA != "W6" {
		t.Errorf("airline must stay: %+v", s)
	}
}

func TestTick_BoardAgreeingKeepsCityNames(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrBUD}}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, r, filter.Filter{}, &clock{t: time.Now()})
	tr.o.BoardRoute = &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", OriginICAO: "EGLL", DestIATA: "BUD", DestICAO: "LHBP"}, ok: true}
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Origin != "LHR" || s.Dest != "BUD" || s.OriginName != "London" || s.DestName != "Budapest" {
		t.Errorf("a board that agrees must keep the city names: %+v", s)
	}
}

func TestTick_BoardRouteBeatsImplausibleCheckAndOutage(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": agpTFS}, errFor: map[string]bool{"WZZ2": true}}
	p := &fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45), airborne("b", "WZZ2", 51.49, -0.45)}}
	tr := newTracker(p, r, filter.Filter{}, &clock{t: time.Now()})
	tr.o.BoardRoute = &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", DestIATA: "WAW"}, ok: true}
	tr.Tick(t.Context())
	l, _ := tr.List()
	for _, s := range l {
		if s.Origin != "LHR" || s.Dest != "WAW" {
			t.Errorf("board route must apply to every listed aircraft, even when adsbdb fails: %+v", s)
		}
	}
}

func TestTick_FilterSeesBoardRoute(t *testing.T) {
	f, err := filter.Parse("", "WAW", "")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrBUD}}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, r, f, &clock{t: time.Now()})
	tr.o.BoardRoute = &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", DestIATA: "WAW"}, ok: true}
	tr.Tick(t.Context())
	if s, _ := tr.Current(); !s.Found || s.Dest != "WAW" {
		t.Errorf("the filter must judge the board's route, not the stale one: %+v", s)
	}
}

func TestPaidRoute_RememberedBoardRouteBeatsFreeRoute(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, false, true)
	rig.rts.byCallsign["WZZ1"] = lhrBUD
	brd := &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", DestIATA: "WAW"}, ok: true}
	rig.tr.o.BoardRoute = brd
	rig.tick(t)
	// The row ages out of the board mid-pass: the stale free route must not come back.
	brd.ok = false
	rig.ck.t = rig.ck.t.Add(time.Minute)
	if s := rig.tick(t); s.Dest != "WAW" || rig.inner.calls() != 0 {
		t.Errorf("remembered board route must win: calls=%d %+v", rig.inner.calls(), s)
	}
}

func TestPaidRoute_RememberedAeroAPIRouteBeatsLaterBoardRow(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, false, true)
	brd := &fakeBoardRoute{}
	rig.tr.o.BoardRoute = brd
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	rig.tick(t)
	// Another leg under the same callsign shows up on the board mid-pass.
	brd.r, brd.ok = flightstatus.FlightRoute{OriginIATA: "LHR", DestIATA: "WAW"}, true
	rig.ck.t = rig.ck.t.Add(time.Minute)
	if s := rig.tick(t); s.Origin != "AGP" || s.Dest != "LGW" || rig.inner.calls() != 1 {
		t.Errorf("the route found this pass must not flap: calls=%d %+v", rig.inner.calls(), s)
	}
}

func TestPaidRoute_UnknownFlightCostsOneCallPerPassWithDelayOn(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "PRIV9", 51.48, -0.45)}, true, true)
	rig.inner.ok = false // AeroAPI has no such flight
	rig.tick(t)
	for i := 0; i < 20; i++ { // seen every minute, past the 15-minute cache
		rig.ck.t = rig.ck.t.Add(time.Minute)
		rig.tick(t)
	}
	if rig.inner.calls() != 1 {
		t.Errorf("a flight AeroAPI does not know costs one call per pass, got %d", rig.inner.calls())
	}
}

// lhrWAW is a hexdb-style route: codes and coordinates, no airline or names.
var lhrWAW = routes.Route{OriginIATA: "LHR", OriginICAO: "EGLL", DestIATA: "WAW", DestICAO: "EPWA",
	HasCoords: true, OriginLat: 51.47, OriginLon: -0.46, DestLat: 52.17, DestLon: 20.97}

func fallbackTracker(free, fb *fakeRoutes, f filter.Filter) *Tracker {
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, free, f, &clock{t: time.Now()})
	tr.o.FallbackRoutes = fb
	return tr
}

func TestTick_FallbackFillsUnknownRoute(t *testing.T) {
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{}, fb, filter.Filter{})
	tr.Tick(t.Context())
	s, _ := tr.Current()
	// hexdb sends no city names; the embedded airport table names the ends.
	if s.Origin != "LHR" || s.Dest != "WAW" || s.OriginName != "London" || s.DestName != "Warsaw" {
		t.Errorf("fallback route must fill an unknown one, named from the airport table: %+v", s)
	}
}

func TestTick_FallbackFillsAfterImplausibleDrop(t *testing.T) {
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": agpTFS}}, fb, filter.Filter{})
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Origin != "LHR" || s.Dest != "WAW" || s.DestName != "Warsaw" {
		t.Errorf("fallback route must replace an implausible one: %+v", s)
	}
	if s.Airline != "Wizz Air" || s.AirlineIATA != "W6" {
		t.Errorf("airline from the free route must stay: %+v", s)
	}
}

func TestTick_FallbackImplausibleNotShown(t *testing.T) {
	far := agpTFS
	far.Airline, far.AirlineIATA, far.OriginName, far.DestName = "", "", "", ""
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": far}}
	tr := fallbackTracker(&fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": agpTFS}}, fb, filter.Filter{})
	tr.Tick(t.Context())
	s, _ := tr.Current()
	if s.Origin != "" || s.Dest != "" {
		t.Errorf("an implausible fallback route must not be shown: %+v", s)
	}
	if s.Airline != "Wizz Air" {
		t.Errorf("airline must stay: %+v", s)
	}
}

// The fallback is asked for every aircraft (for the source log), but a
// plausible free route still decides what is shown.
func TestTick_PlausibleFreeRouteBeatsFallback(t *testing.T) {
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrBUD}}, fb, filter.Filter{})
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Dest != "BUD" {
		t.Errorf("a plausible free route must win over the fallback: %+v", s)
	}
}

func TestTick_BoardBeatsFallback(t *testing.T) {
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{}, fb, filter.Filter{})
	tr.o.BoardRoute = &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", DestIATA: "BUD"}, ok: true}
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Dest != "BUD" {
		t.Errorf("the board must win over the fallback: %+v", s)
	}
}

func TestTick_FallbackErrorDoesNotFlipRouteFailing(t *testing.T) {
	fb := &fakeRoutes{errFor: map[string]bool{"WZZ1": true}}
	tr := fallbackTracker(&fakeRoutes{}, fb, filter.Filter{})
	tr.Tick(t.Context())
	if tr.routeFailing || fb.calls != 1 {
		t.Errorf("a fallback outage must not mark route lookup failing: calls=%d", fb.calls)
	}
	if s, _ := tr.Current(); !s.Found || s.Origin != "" {
		t.Errorf("aircraft must still show, without a route: %+v", s)
	}
}

func TestTick_FilterSeesFallbackRoute(t *testing.T) {
	f, err := filter.Parse("", "WAW", "")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{}, fb, f)
	tr.Tick(t.Context())
	if s, _ := tr.Current(); !s.Found || s.Dest != "WAW" {
		t.Errorf("the filter must judge the fallback route: %+v", s)
	}
}

// blockingRoutes stalls until the caller gives up, like an upstream that
// accepts connections but never answers.
type blockingRoutes struct{ calls atomic.Int32 }

func (b *blockingRoutes) Route(ctx context.Context, cs string) (routes.Route, bool, error) {
	b.calls.Add(1)
	<-ctx.Done()
	return routes.Route{}, false, ctx.Err()
}

func TestTick_StalledFallbackCostsOneTimeoutPerTick(t *testing.T) {
	var acs []provider.Aircraft
	for i := range 10 {
		acs = append(acs, airborne(fmt.Sprintf("h%d", i), fmt.Sprintf("WZZ%d", i), 51.48+float64(i)*0.001, -0.45))
	}
	tr := newTracker(&fakeProvider{acs: acs}, &fakeRoutes{}, filter.Filter{}, &clock{t: time.Now()})
	tr.o.RouteTimeout = 50 * time.Millisecond
	fb := &blockingRoutes{}
	tr.o.FallbackRoutes = fb
	start := time.Now()
	tr.Tick(t.Context())
	if d := time.Since(start); d > 3*tr.o.RouteTimeout {
		t.Errorf("a stalled fallback must share one deadline per tick, took %v (%d calls)", d, fb.calls.Load())
	}
	if l, _ := tr.List(); len(l) != 10 {
		t.Errorf("every aircraft must still be listed: %d", len(l))
	}
}

// stallOnce blocks for one callsign until the caller gives up and answers
// the rest from its map, so a real routes.Cache can be warmed around it.
type stallOnce struct {
	stall string
	byCs  map[string]routes.Route
}

func (s *stallOnce) Route(ctx context.Context, cs string) (routes.Route, bool, error) {
	if cs == s.stall {
		<-ctx.Done()
		return routes.Route{}, false, ctx.Err()
	}
	r, ok := s.byCs[cs]
	return r, ok, nil
}

func TestTick_SpentFallbackBudgetKeepsCachedRoutes(t *testing.T) {
	ck := &clock{t: time.Now()}
	inner := &stallOnce{byCs: map[string]routes.Route{"WZZ2": lhrWAW}}
	fb := routes.NewCache(inner, time.Hour, time.Hour, time.Minute, ck.now)
	if _, ok, err := fb.Route(t.Context(), "WZZ2"); !ok || err != nil {
		t.Fatal("warm-up failed")
	}
	inner.stall = "WZZ1"
	f, err := filter.Parse("", "WAW", "")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45), airborne("b", "WZZ2", 51.49, -0.45)}}
	tr := newTracker(p, &fakeRoutes{}, f, ck)
	tr.o.RouteTimeout = 30 * time.Millisecond
	tr.o.FallbackRoutes = fb
	tr.Tick(t.Context())
	l, _ := tr.List()
	if len(l) != 1 || l[0].Callsign != "WZZ2" || l[0].Dest != "WAW" {
		t.Errorf("a cached fallback route must survive a spent tick budget: %+v", l)
	}
}

func TestTick_SlowPrimaryDoesNotSpendFallbackBudget(t *testing.T) {
	ck := &clock{t: time.Now()}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, &stallOnce{stall: "WZZ1"}, filter.Filter{}, ck)
	tr.o.RouteTimeout = 30 * time.Millisecond
	tr.o.FallbackRoutes = routes.NewCache(&fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}, time.Hour, time.Hour, time.Minute, ck.now)
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Dest != "WAW" {
		t.Errorf("a stalled primary source must leave the fallback its own budget: %+v", s)
	}
}

type recorderSpy struct{ batches [][]PassRecord }

func (r *recorderSpy) Record(recs []PassRecord) { r.batches = append(r.batches, recs) }

func (r *recorderSpy) last(t *testing.T) []PassRecord {
	t.Helper()
	if len(r.batches) == 0 {
		t.Fatal("nothing recorded")
	}
	return r.batches[len(r.batches)-1]
}

func TestRecord_OneRecordPerListedAircraft(t *testing.T) {
	p := &fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.50, -0.45), airborne("b", "WZZ2", 51.48, -0.45)}}
	tr := newTracker(p, &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ2": lhrBUD}}, filter.Filter{}, &clock{t: time.Now()})
	spy := &recorderSpy{}
	tr.o.Recorder = spy
	tr.Tick(t.Context())
	recs := spy.last(t)
	if len(recs) != 2 || recs[0].Hex != "b" || !recs[0].Nearest || recs[1].Nearest {
		t.Fatalf("one record per aircraft, nearest first and only it marked: %+v", recs)
	}
	r := recs[0]
	if r.FirstSeen.IsZero() || r.LastSeen.IsZero() || r.Callsign != "WZZ2" || r.Airline != "Wizz Air" {
		t.Errorf("pass identity and context must be filled: %+v", r)
	}
	if r.ADSBDB != (SourceAnswer{StatusHit, "LHR-BUD"}) || r.Board.Status != StatusOff || r.HexDB.Status != StatusOff ||
		r.AeroAPI.Status != StatusOff || r.ShownSource != SourceADSBDB || r.ShownRoute != "LHR-BUD" {
		t.Errorf("adsbdb shown, other sources off: %+v", r)
	}
	if recs[1].ADSBDB.Status != StatusUnknown || recs[1].ShownSource != SourceNone || recs[1].ShownRoute != "" {
		t.Errorf("an unknown route is recorded as such: %+v", recs[1])
	}
}

func TestRecord_EverySourceAnswerAndTheShownOne(t *testing.T) {
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": agpTFS}}, fb, filter.Filter{})
	tr.o.BoardRoute = &fakeBoardRoute{}
	spy := &recorderSpy{}
	tr.o.Recorder = spy
	tr.Tick(t.Context())
	r := spy.last(t)[0]
	if r.ADSBDB != (SourceAnswer{StatusImplausible, "AGP-TFS"}) || r.Board.Status != StatusMiss ||
		r.HexDB != (SourceAnswer{StatusHit, "LHR-WAW"}) || r.ShownSource != SourceHexDB || r.ShownRoute != "LHR-WAW" {
		t.Errorf("implausible adsbdb, board miss, hexdb shown: %+v", r)
	}
}

func TestRecord_SourceStatuses(t *testing.T) {
	far := agpTFS
	far.Airline, far.AirlineIATA = "", ""
	cases := []struct {
		name  string
		free  *fakeRoutes
		fb    *fakeRoutes
		cs    string
		check func(PassRecord) bool
	}{
		{"no callsign", &fakeRoutes{}, &fakeRoutes{}, "",
			func(r PassRecord) bool {
				return r.ADSBDB.Status == StatusNone && r.Board.Status == StatusNone && r.HexDB.Status == StatusNone
			}},
		{"adsbdb error", &fakeRoutes{errFor: map[string]bool{"WZZ1": true}}, &fakeRoutes{}, "WZZ1",
			func(r PassRecord) bool { return r.ADSBDB.Status == StatusError && r.HexDB.Status == StatusUnknown }},
		{"hexdb error", &fakeRoutes{}, &fakeRoutes{errFor: map[string]bool{"WZZ1": true}}, "WZZ1",
			func(r PassRecord) bool { return r.HexDB.Status == StatusError }},
		{"adsbdb cooldown", &fakeRoutes{errAll: routes.ErrCooldown}, &fakeRoutes{}, "WZZ1",
			func(r PassRecord) bool { return r.ADSBDB.Status == StatusCooldown }},
		{"adsbdb gate closed", &fakeRoutes{errAll: routes.ErrRateLimited}, &fakeRoutes{}, "WZZ1",
			func(r PassRecord) bool { return r.ADSBDB.Status == StatusCooldown }},
		{"hexdb cooldown", &fakeRoutes{}, &fakeRoutes{errAll: routes.ErrCooldown}, "WZZ1",
			func(r PassRecord) bool { return r.HexDB.Status == StatusCooldown }},
		{"hexdb implausible", &fakeRoutes{}, &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": far}}, "WZZ1",
			func(r PassRecord) bool {
				return r.HexDB == SourceAnswer{StatusImplausible, "AGP-TFS"} && r.ShownSource == SourceNone
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := airborne("a", tc.cs, 51.48, -0.45)
			tr := newTracker(&fakeProvider{acs: []provider.Aircraft{ac}}, tc.free, filter.Filter{}, &clock{t: time.Now()})
			tr.o.FallbackRoutes = tc.fb
			tr.o.BoardRoute = &fakeBoardRoute{}
			spy := &recorderSpy{}
			tr.o.Recorder = spy
			tr.Tick(t.Context())
			if r := spy.last(t)[0]; !tc.check(r) {
				t.Errorf("unexpected record: %+v", r)
			}
		})
	}
}

func TestRecord_SpentFallbackBudgetIsSkipped(t *testing.T) {
	ck := &clock{t: time.Now()}
	p := &fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45), airborne("b", "WZZ2", 51.49, -0.45)}}
	tr := newTracker(p, &fakeRoutes{}, filter.Filter{}, ck)
	tr.o.RouteTimeout = 20 * time.Millisecond
	tr.o.FallbackRoutes = routes.NewCache(&stallOnce{stall: "WZZ1"}, time.Hour, time.Hour, time.Minute, ck.now)
	spy := &recorderSpy{}
	tr.o.Recorder = spy
	tr.Tick(t.Context())
	recs := spy.last(t)
	if recs[0].HexDB.Status != StatusSkipped || recs[1].HexDB.Status != StatusSkipped {
		t.Errorf("the stalled lookup and the one after it ran out of budget: %+v", recs)
	}
}

func TestRecord_PaidRouteAndItsMemo(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, false, true)
	spy := &recorderSpy{}
	rig.tr.o.Recorder = spy
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true}, true
	rig.tick(t)
	r := spy.last(t)[0]
	if r.AeroAPI != (SourceAnswer{StatusHit, "AGP-LGW"}) || r.ShownSource != SourceAeroAPI || r.ShownRoute != "AGP-LGW" {
		t.Errorf("paid route recorded and shown: %+v", r)
	}
	rig.ck.t = rig.ck.t.Add(time.Minute)
	rig.tick(t)
	r = spy.last(t)[0]
	if r.AeroAPI.Status != StatusHit || r.ShownSource != SourceAeroAPI || rig.inner.calls() != 1 {
		t.Errorf("a remembered paid route keeps its source: calls=%d %+v", rig.inner.calls(), r)
	}
}

func TestRecord_PaidUnknownAndNotAsked(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45), airborne("b", "WZZ2", 51.50, -0.45)}, false, true)
	spy := &recorderSpy{}
	rig.tr.o.Recorder = spy
	rig.tick(t)
	recs := spy.last(t)
	if recs[0].AeroAPI.Status != StatusUnknown || recs[1].AeroAPI.Status != StatusNotAsked {
		t.Errorf("nearest asked and unknown, the other not asked: %+v", recs)
	}
}

func TestRecord_BoardRouteShownAndRemembered(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, false, true)
	brd := &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "LHR", DestIATA: "WAW"}, ok: true}
	rig.tr.o.BoardRoute = brd
	spy := &recorderSpy{}
	rig.tr.o.Recorder = spy
	rig.tick(t)
	brd.ok = false
	rig.ck.t = rig.ck.t.Add(time.Minute)
	rig.tick(t)
	r := spy.last(t)[0]
	if r.Board.Status != StatusMiss || r.ShownSource != SourceBoard || r.ShownRoute != "LHR-WAW" {
		t.Errorf("a remembered board route keeps its source while the board now misses: %+v", r)
	}
}

func TestTick_ComparisonLookupDoesNotStarveNeededOne(t *testing.T) {
	ck := &clock{t: time.Now()}
	// "a" is named by a plausible adsbdb route, so its hexdb answer is only
	// for the log; it stalls. "b" needs hexdb and must still get it.
	p := &fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45), airborne("b", "WZZ2", 51.49, -0.45)}}
	tr := newTracker(p, &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrBUD}}, filter.Filter{}, ck)
	tr.o.RouteTimeout = 30 * time.Millisecond
	tr.o.FallbackRoutes = routes.NewCache(&stallOnce{stall: "WZZ1", byCs: map[string]routes.Route{"WZZ2": lhrWAW}}, time.Hour, time.Hour, time.Minute, ck.now)
	tr.Tick(t.Context())
	l, _ := tr.List()
	if len(l) != 2 || l[0].Dest != "BUD" || l[1].Dest != "WAW" {
		t.Errorf("a stalled comparison-only lookup must not spend the budget a needed one relies on: %+v", l)
	}
}

func TestTick_ComparisonStallBetweenNeededLookupsCostsNothing(t *testing.T) {
	// A needs hexdb (fast), B only compares (stalls), C needs hexdb and is
	// nearest: C must get its free route and no paid call may happen.
	rig := newPaidRig([]provider.Aircraft{
		airborne("c", "WZZ3", 51.471, -0.45),
		airborne("a", "WZZ1", 51.48, -0.45),
		airborne("b", "WZZ2", 51.49, -0.45),
	}, false, true)
	rig.rts.byCallsign["WZZ2"] = lhrBUD
	rig.tr.o.RouteTimeout = 30 * time.Millisecond
	rig.tr.o.Recorder = &recorderSpy{}
	// Provider order puts "c" first; make A and B come before C in the loop.
	p := rig.tr.o.Provider.(*fakeProvider)
	p.acs = []provider.Aircraft{p.acs[1], p.acs[2], p.acs[0]}
	rig.tr.o.FallbackRoutes = routes.NewCache(&stallOnce{stall: "WZZ2",
		byCs: map[string]routes.Route{"WZZ1": lhrWAW, "WZZ3": lhrWAW}}, time.Hour, time.Hour, time.Minute, rig.ck.now)
	s := rig.tick(t)
	if s.Callsign != "WZZ3" || s.Dest != "WAW" || rig.inner.calls() != 0 {
		t.Errorf("the nearest aircraft must get its free route without a paid call: calls=%d %+v", rig.inner.calls(), s)
	}
}

func TestTick_NoComparisonLookupsWithoutRecorder(t *testing.T) {
	fb := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrWAW}}
	tr := fallbackTracker(&fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": lhrBUD}}, fb, filter.Filter{})
	tr.Tick(t.Context())
	if fb.calls != 0 {
		t.Errorf("without a source log, an aircraft with a route must not cost a fallback lookup: %d", fb.calls)
	}
}

func TestRecord_AeroAPIRouteFromDelayLookup(t *testing.T) {
	rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, true, true)
	rig.rts.byCallsign["WZZ1"] = lhrBUD
	spy := &recorderSpy{}
	rig.tr.o.Recorder = spy
	late := 20 * 60
	rig.inner.fl, rig.inner.ok = flightstatus.Flight{Route: agpLGW, HasRoute: true, Status: flightstatus.Classify(late/60, "arrival"), HasStatus: true}, true
	s := rig.tick(t)
	r := spy.last(t)[0]
	if s.Dest != "BUD" || r.ShownSource != SourceADSBDB || rig.inner.calls() != 1 {
		t.Fatalf("display unchanged, one paid call for the delay: calls=%d %+v", rig.inner.calls(), s)
	}
	if r.AeroAPI != (SourceAnswer{StatusHit, "AGP-LGW"}) {
		t.Errorf("the route the delay lookup paid for must be recorded: %+v", r.AeroAPI)
	}
}

func TestTick_AgreeingBoardKeepsNamesOfImplausibleFreeRoute(t *testing.T) {
	r := &fakeRoutes{byCallsign: map[string]routes.Route{"WZZ1": agpTFS}}
	tr := newTracker(&fakeProvider{acs: []provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}}, r, filter.Filter{}, &clock{t: time.Now()})
	tr.o.BoardRoute = &fakeBoardRoute{r: flightstatus.FlightRoute{OriginIATA: "AGP", DestIATA: "TFS"}, ok: true}
	tr.Tick(t.Context())
	if s, _ := tr.Current(); s.Origin != "AGP" || s.OriginName != "Malaga" || s.DestName != "Tenerife" {
		t.Errorf("a board route that agrees must keep adsbdb's city names: %+v", s)
	}
}

func TestRecord_FailedDelayLookupIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{errors.New("down"), StatusError}, {flightstatus.ErrBudget, StatusSkipped},
		{flightstatus.ErrCooldown, StatusCooldown}} {
		rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, true, true)
		rig.rts.byCallsign["WZZ1"] = lhrBUD
		spy := &recorderSpy{}
		rig.tr.o.Recorder = spy
		rig.inner.err = tc.err
		rig.tick(t)
		if r := spy.last(t)[0]; r.AeroAPI.Status != tc.want || rig.inner.calls() != 1 {
			t.Errorf("%v: delay lookup outcome must be recorded as %s: calls=%d %+v", tc.err, tc.want, rig.inner.calls(), r.AeroAPI)
		}
	}
}

// The route-only paid call (no delay wanted) records a cooldown as such.
func TestRecord_PaidRouteCooldownIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{errors.New("down"), StatusError}, {flightstatus.ErrCooldown, StatusCooldown}, {flightstatus.ErrBudget, StatusSkipped}} {
		rig := newPaidRig([]provider.Aircraft{airborne("a", "WZZ1", 51.48, -0.45)}, false, true)
		spy := &recorderSpy{}
		rig.tr.o.Recorder = spy
		rig.inner.err = tc.err
		rig.tick(t)
		if r := spy.last(t)[0]; r.AeroAPI.Status != tc.want || rig.inner.calls() != 1 {
			t.Errorf("%v: want %s, calls=%d %+v", tc.err, tc.want, rig.inner.calls(), r.AeroAPI)
		}
	}
}

func TestPositionFailureHookOnlyForBadBody(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{fmt.Errorf("%w: quota", provider.ErrBadBody), 1}, {errors.New("provider: status 503"), 0}} {
		var got []string
		tr := newTracker(&fakeProvider{err: tc.err}, &fakeRoutes{}, filter.Filter{}, &clock{t: time.Now()})
		tr.o.PositionFailure = func(msg string) { got = append(got, msg) }
		tr.Tick(t.Context())
		if len(got) != tc.want {
			t.Errorf("%v: hook calls = %v, want %d", tc.err, got, tc.want)
		}
	}
}
