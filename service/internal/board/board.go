package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	statePath string
}

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
	Year  int        `json:"year"`
	Month time.Month `json:"month"`
	Calls int        `json:"calls"`
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
		b.year, b.month, b.calls = st.Year, st.Month, st.Calls
	}
	return nil
}

// reserve counts n calls against the month, rolling the counter over at
// the UTC month boundary; false when the cap would be exceeded.
func (b *Board) reserve(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	y, m, _ := b.now().UTC().Date()
	if y != b.year || m != b.month {
		b.year, b.month, b.calls = y, m, 0
	}
	if b.calls+n > b.cap {
		return false
	}
	b.calls += n
	if b.statePath != "" {
		if err := writeState(b.statePath, counterState{b.year, b.month, b.calls}); err != nil {
			b.logger.Warn("board state not saved", "err", err)
		}
	}
	return true
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
	if airport == "" || len(rows) == 0 || now.Sub(fetchedAt) > b.maxAge {
		return flightstatus.Status{}, false, nil
	}
	r, ok := Match(rows, airport, q, now)
	if !ok {
		return flightstatus.Status{}, false, nil
	}
	arrival := sameAirportAny(airportCodes(rows, airport), r.ArrIATA, r.ArrICAO)
	return StatusOf(r, arrival, q.Seen), true, nil
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
	return Stats{Airport: b.airport, Flights: len(b.rows), AgeS: age, CallsMonth: calls, CallsCap: b.cap}
}

// Run refreshes immediately, then whenever the interval has elapsed or the
// configured airport changed, checking once a minute.
func (b *Board) Run(ctx context.Context, airport func() string, interval time.Duration) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	b.run(ctx, airport, interval, t.C)
}

const (
	backoffStart = 5 * time.Minute
	backoffMax   = 2 * time.Hour
)

func (b *Board) run(ctx context.Context, airport func() string, interval time.Duration, tick <-chan time.Time) {
	last := ""
	failures := 0
	var notBefore time.Time
	try := func() {
		want := airport()
		if want == "" && last == "" {
			return
		}
		now := b.now()
		if now.Before(notBefore) {
			return
		}
		b.mu.RLock()
		due := want != last || b.fetchedAt.IsZero() || now.Sub(b.fetchedAt) >= interval
		b.mu.RUnlock()
		if !due {
			return
		}
		err := b.Refresh(ctx, want)
		switch {
		case errors.Is(err, ErrCap):
			// Nothing will succeed until the month rolls over: say so once and
			// stay quiet instead of warning every minute.
			y, m, _ := now.UTC().Date()
			notBefore = time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
			b.logger.Warn("board refresh refused: monthly call cap spent", "airport", want, "resumes", notBefore.Format(time.RFC3339))
			return
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			// Each attempt costs two counted calls, so a provider outage or a
			// bad key must not be retried every minute.
			failures++
			delay := backoffStart << (failures - 1)
			if failures > 6 || delay > backoffMax {
				delay = backoffMax
			}
			if delay > interval {
				delay = interval
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
