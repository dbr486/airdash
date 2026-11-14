package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"airdash/internal/flightstatus"
)

var ErrCap = errors.New("board: monthly call cap spent")

type Fetcher interface {
	Fetch(ctx context.Context, airport string, arrivals bool) ([]Row, error)
}

// Board holds the latest departures and arrivals of one airport and
// answers delay queries from it without any network call. A monthly call
// counter guards the key's quota before the provider does; it can be
// persisted so a restart does not forget the month's spend.
type Board struct {
	fetch  Fetcher
	cap    int
	maxAge time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu        sync.RWMutex
	airport   string
	rows      []Row
	fetchedAt time.Time
	month     time.Month
	year      int
	calls     int
	capHitAt  time.Time
	statePath string
	onRefuse  func(subject string)
	onFail    func(subject, msg string)
	// capFn, when set, replaces the constructor cap with a live one (the
	// settings value) and makes the refresh interval follow it.
	capFn func() int
}

const (
	minInterval = 15 * time.Minute
	minStale    = 30 * time.Minute
)

type Stats struct {
	Airport    string
	Flights    int
	AgeS       int // -1 before the first successful refresh
	CallsMonth int
	CallsCap   int
}

// New builds a board that refuses more than callsPerMonth upstream calls
// and treats rows older than maxAge as unknown (so a paid fallback can
// take over when refreshes keep failing).
func New(f Fetcher, callsPerMonth int, maxAge time.Duration, now func() time.Time, logger *slog.Logger) *Board {
	return &Board{fetch: f, cap: callsPerMonth, maxAge: maxAge, now: now, logger: logger}
}

type counterState struct {
	Year     int        `json:"year"`
	Month    time.Month `json:"month"`
	Calls    int        `json:"calls"`
	CapHitAt time.Time  `json:"cap_hit_at"`
}

// Persist loads this month's call count from path (if any) and writes it
// back on every reservation, so the cap survives restarts and deploys.
func (b *Board) Persist(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statePath = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st counterState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("board state: %w", err)
	}
	if y, m, _ := b.now().UTC().Date(); y == st.Year && m == st.Month {
		b.year, b.month, b.calls, b.capHitAt = st.Year, st.Month, st.Calls, st.CapHitAt
	}
	return nil
}

// reserve counts n calls against the month, rolling the counter over at
// the UTC month boundary; false when the cap would be exceeded.
func (b *Board) reserve(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollMonth()
	if b.calls+n > b.capNow() {
		if b.capHitAt.IsZero() {
			b.capHitAt = b.now().UTC()
			b.save()
		}
		return false
	}
	b.calls += n
	b.save()
	return true
}

// capNow is the cap in force: the live one when set. capFn must not take
// b.mu, which callers here may hold.
func (b *Board) capNow() int {
	if b.capFn != nil {
		return b.capFn()
	}
	return b.cap
}

// SetCapFunc makes the board read its monthly cap live, so a cap changed in
// Settings applies to the next refresh and reshapes the interval.
func (b *Board) SetCapFunc(fn func() int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.capFn = fn
}

// Interval spreads the calls left this month over the time left, two
// calls per refresh, so any cap lasts the month: about 96 min for 900
// calls at the start of a 30-day month. Never below 15 min; 0 when the cap
// cannot fund another refresh.
func (b *Board) Interval(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.intervalLocked(now)
}

func (b *Board) intervalLocked(now time.Time) time.Duration {
	y, m, _ := now.UTC().Date()
	calls := b.calls
	if y != b.year || m != b.month {
		calls = 0
	}
	refreshes := (b.capNow() - calls) / 2
	if refreshes < 1 {
		return 0
	}
	left := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC).Sub(now)
	return max(left/time.Duration(refreshes), minInterval)
}

// Schedule reports the current interval and whether the cap is reached,
// for the limits page.
func (b *Board) Schedule() (time.Duration, bool) {
	iv := b.Interval(b.now())
	return iv, iv == 0
}

// staleAfter is how old the rows may get before they count as unknown:
// two refresh intervals with a live cap (rows from one missed refresh are
// still good), else the constructor's maxAge.
func (b *Board) staleAfter(now time.Time) time.Duration {
	b.mu.RLock()
	live := b.capFn != nil
	b.mu.RUnlock()
	if !live {
		return b.maxAge
	}
	iv := b.Interval(now)
	if iv == 0 {
		return b.maxAge
	}
	return max(2*iv, minStale)
}

// rollMonth starts a new month's count at the UTC boundary. Caller holds mu.
func (b *Board) rollMonth() {
	y, m, _ := b.now().UTC().Date()
	if y != b.year || m != b.month {
		b.year, b.month, b.calls, b.capHitAt = y, m, 0, time.Time{}
	}
}

func (b *Board) save() {
	if b.statePath == "" {
		return
	}
	if err := writeState(b.statePath, counterState{b.year, b.month, b.calls, b.capHitAt}); err != nil {
		b.logger.Warn("board state not saved", "err", err)
	}
}

// CapHitAt is when this month's cap first refused a refresh; zero if not.
func (b *Board) CapHitAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollMonth()
	return b.capHitAt
}

// OnRefuse registers fn to hear the airport of every refresh the cap
// refuses; OnFail hears errors AirLabs sent inside a 200 body (ErrAPI).
func (b *Board) OnRefuse(fn func(subject string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onRefuse = fn
}

func (b *Board) OnFail(fn func(subject, msg string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onFail = fn
}

func writeState(path string, st counterState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".board-calls.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Refresh downloads both directions. The two calls are counted up front so
// a refused refresh costs nothing, and a partial failure keeps the old
// board rather than publishing half of one. An empty airport clears it.
func (b *Board) Refresh(ctx context.Context, airport string) error {
	if airport == "" {
		b.mu.Lock()
		b.airport, b.rows, b.fetchedAt = "", nil, time.Time{}
		b.mu.Unlock()
		return nil
	}
	if !b.reserve(2) {
		b.mu.RLock()
		fn := b.onRefuse
		b.mu.RUnlock()
		if fn != nil {
			fn(airport)
		}
		return ErrCap
	}
	dep, err := b.fetch.Fetch(ctx, airport, false)
	if err != nil {
		return fmt.Errorf("departures: %w", err)
	}
	arr, err := b.fetch.Fetch(ctx, airport, true)
	if err != nil {
		return fmt.Errorf("arrivals: %w", err)
	}
	rows := append(append(make([]Row, 0, len(dep)+len(arr)), dep...), arr...)
	b.mu.Lock()
	b.airport, b.rows, b.fetchedAt = airport, rows, b.now()
	b.mu.Unlock()
	return nil
}

// Status implements flightstatus.Lookup from the cached board: a miss is
// (Status{}, false, nil), never an error, so the chained fallback runs.
// A board older than maxAge is a miss too: hours-old estimates are worse
// than asking the paid source.
func (b *Board) Status(_ context.Context, q flightstatus.Query) (flightstatus.Status, bool, error) {
	b.mu.RLock()
	airport, rows, fetchedAt := b.airport, b.rows, b.fetchedAt
	b.mu.RUnlock()
	now := b.now()
	if airport == "" || len(rows) == 0 || now.Sub(fetchedAt) > b.staleAfter(now) {
		return flightstatus.Status{}, false, nil
	}
	r, ok := Match(rows, airport, q, now)
	if !ok {
		return flightstatus.Status{}, false, nil
	}
	arrival := sameAirportAny(airportCodes(rows, airport), r.ArrIATA, r.ArrICAO)
	return StatusOf(r, arrival, q.Seen), true, nil
}

// Route gives the route of the one board row whose ICAO flight id equals
// the callsign, without any network call. The aircraft's direction is
// unknown here (that is what is being asked), so both ends are tried and
// two plausible rows fail closed: a wrong route would be shown as fact,
// and the caller can still pay for the exact one. A stale board is a miss
// for the same reason as in Status.
func (b *Board) Route(q flightstatus.Query, now time.Time) (flightstatus.FlightRoute, bool) {
	b.mu.RLock()
	airport, rows, fetchedAt := b.airport, b.rows, b.fetchedAt
	b.mu.RUnlock()
	cs := strings.ToUpper(strings.TrimSpace(q.Callsign))
	if cs == "" || airport == "" || len(rows) == 0 || now.Sub(fetchedAt) > b.staleAfter(now) {
		return flightstatus.FlightRoute{}, false
	}
	// Unlike Status, which may report on a flight still at the gate, the route
	// names an aircraft already airborne, so only a row for the leg flying
	// now counts. Tag-on flights reuse one number for several legs.
	codes := airportCodes(rows, airport)
	var hit Row
	n := 0
	for _, r := range rows {
		if r.FlightICAO == cs && airborneNow(r, sameAirportAny(codes, r.ArrIATA, r.ArrICAO), now) {
			hit = r
			n++
		}
	}
	if n != 1 {
		return flightstatus.FlightRoute{}, false
	}
	return RouteOf(hit), true
}

func (b *Board) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	age := -1
	if !b.fetchedAt.IsZero() {
		age = int(b.now().Sub(b.fetchedAt).Seconds())
	}
	calls := b.calls
	if y, m, _ := b.now().UTC().Date(); y != b.year || m != b.month {
		calls = 0
	}
	return Stats{Airport: b.airport, Flights: len(b.rows), AgeS: age, CallsMonth: calls, CallsCap: b.capNow()}
}

// Run refreshes immediately, then whenever Interval has elapsed or the
// configured airport changed, checking once a minute.
func (b *Board) Run(ctx context.Context, airport func() string) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	b.run(ctx, airport, b.Interval, t.C)
}

const (
	backoffStart = 5 * time.Minute
	backoffMax   = 2 * time.Hour
)

func (b *Board) run(ctx context.Context, airport func() string, interval func(time.Time) time.Duration, tick <-chan time.Time) {
	last := ""
	failures := 0
	var notBefore time.Time
	capWarned := "" // the month the cap warning was last logged
	// capBlocked remembers that the last attempt hit the cap: once a raise
	// or a new month frees calls again, the next tick refreshes at once
	// instead of waiting a spread interval from the refresh before the block.
	capBlocked := false
	try := func() {
		want := airport()
		if want == "" && last == "" {
			return
		}
		now := b.now()
		if now.Before(notBefore) {
			return
		}
		// A spent cap gives a zero interval, so every tick is due and reserve
		// refuses it without I/O; with capBlocked, a cap raised in Settings or
		// a new month resumes refreshes within a minute.
		iv := interval(now)
		b.mu.RLock()
		due := want != last || b.fetchedAt.IsZero() || now.Sub(b.fetchedAt) >= iv || (capBlocked && iv > 0)
		b.mu.RUnlock()
		if !due {
			return
		}
		err := b.Refresh(ctx, want)
		capBlocked = errors.Is(err, ErrCap)
		switch {
		case errors.Is(err, ErrCap):
			if month := now.UTC().Format("2006-01"); capWarned != month {
				capWarned = month
				b.logger.Warn("board refresh refused: monthly call cap spent", "airport", want)
			}
			return
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			b.mu.RLock()
			fn := b.onFail
			b.mu.RUnlock()
			// Transport and status failures are already metered at the
			// client; only an error hidden in a 200 body needs reporting.
			if fn != nil && errors.Is(err, ErrAPI) {
				fn(want, err.Error())
			}
			// Each attempt costs two counted calls, so a provider outage or a
			// bad key must not be retried every minute.
			failures++
			delay := backoffStart << (failures - 1)
			if failures > 6 || delay > backoffMax {
				delay = backoffMax
			}
			if iv > 0 && delay > iv {
				delay = iv
			}
			notBefore = now.Add(delay)
			b.logger.Warn("board refresh failed", "airport", want, "err", err, "retry_in", delay)
			return
		}
		failures, notBefore = 0, time.Time{}
		last = want
		if want == "" {
			b.logger.Info("board cleared (no airport configured)")
			return
		}
		s := b.Stats()
		b.logger.Info("board refreshed", "airport", want, "flights", s.Flights, "calls_month", s.CallsMonth, "cap", s.CallsCap)
	}
	try()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			try()
		}
	}
}
