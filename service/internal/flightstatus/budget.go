package flightstatus

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
)

var ErrBudget = errors.New("flightstatus: daily lookup budget exhausted")

// Budget caps upstream AeroAPI calls per UTC day so the Personal tier's
// free credit is never exceeded by accident. Persist keeps the day's count
// on disk so a redeploy cannot reset it and overspend.
type Budget struct {
	capFn  func() int
	now    func() time.Time
	logger *slog.Logger

	mu        sync.Mutex
	day       string
	used      int
	capHitAt  time.Time
	warnedOn  string
	statePath string
	// blockedDay is a day whose spend is unknown (the state file could not
	// be read): paid calls stay off until it ends, rather than risk paying
	// again for calls a lost count no longer shows.
	blockedDay string
	saveFailed bool // the last counted call could not be written
	onRefuse   func(subject string)
}

type budgetState struct {
	Day      string    `json:"day"`
	Used     int       `json:"used"`
	CapHitAt time.Time `json:"cap_hit_at"`
}

func NewBudget(capFn func() int, now func() time.Time, logger *slog.Logger) *Budget {
	if logger == nil {
		logger = slog.Default()
	}
	return &Budget{capFn: capFn, now: now, logger: logger}
}

// Persist loads today's count from path (if any) and writes it back on
// every counted call. The write is synchronous on purpose: the file is tiny,
// there are a few dozen writes a day, and a count lost to a crash is spend
// the cap no longer sees.
func (b *Budget) Persist(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statePath = path
	b.roll()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.blockedDay = b.day
		return err
	}
	// Pointers tell a missing field from a zero: a state without a count
	// must not restore "nothing spent".
	var st struct {
		Day      *string   `json:"day"`
		Used     *int      `json:"used"`
		CapHitAt time.Time `json:"cap_hit_at"`
	}
	err = json.Unmarshal(data, &st)
	if err == nil && (st.Day == nil || st.Used == nil || *st.Used < 0) {
		err = errors.New("missing or negative day/used")
	}
	if err == nil {
		_, err = time.Parse("2006-01-02", *st.Day)
	}
	if err != nil {
		b.blockedDay = b.day
		return fmt.Errorf("budget state: %w", err)
	}
	if *st.Day == b.day {
		b.used, b.capHitAt = *st.Used, st.CapHitAt
	}
	return nil
}

// Blocked says why paid calls are refused below the cap, or "" if they
// are not: the limits page shows it instead of a misleading OK.
func (b *Budget) Blocked() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	switch {
	case b.day == b.blockedDay:
		return "call count file unreadable; paid calls off until UTC midnight"
	case b.saveFailed:
		return "call count could not be saved; paid calls refused"
	}
	return ""
}

func (b *Budget) save() error {
	if b.statePath == "" {
		return nil
	}
	data, err := json.Marshal(budgetState{Day: b.day, Used: b.used, CapHitAt: b.capHitAt})
	if err == nil {
		tmp := filepath.Join(filepath.Dir(b.statePath), ".aeroapi-budget.tmp")
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, b.statePath)
		}
	}
	if err != nil {
		b.logger.Warn("delay budget state not saved", "err", err)
	}
	return err
}

func (b *Budget) roll() {
	d := b.now().UTC().Format("2006-01-02")
	if d != b.day {
		b.day, b.used, b.capHitAt = d, 0, time.Time{}
	}
}

func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	limit := b.capFn()
	if b.day == b.blockedDay {
		return false
	}
	if b.used >= limit {
		if limit > 0 && b.capHitAt.IsZero() {
			b.capHitAt = b.now().UTC()
			_ = b.save() // diagnostic only; the refusal stands either way
		}
		if b.warnedOn != b.day && limit > 0 {
			b.warnedOn = b.day
			b.logger.Warn("delay budget exhausted", "cap", limit, "day", b.day)
		}
		return false
	}
	b.used++
	if b.save() != nil {
		// Fail closed: a call whose count is not on disk could be paid for
		// again after a restart today.
		b.used--
		b.saveFailed = true
		return false
	}
	b.saveFailed = false
	return true
}

// CapHitAt is when today's cap first refused a call; zero if it has not.
func (b *Budget) CapHitAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.capHitAt
}

// OnRefuse registers fn to hear the ident of every call the cap refuses.
func (b *Budget) OnRefuse(fn func(subject string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onRefuse = fn
}

func (b *Budget) refused(subject string) {
	b.mu.Lock()
	fn := b.onRefuse
	b.mu.Unlock()
	if fn != nil {
		fn(subject)
	}
}

// Refund gives back one unit consumed by Allow, for a call that turned out
// not to be a genuine attempt against the budget (e.g. an auth failure
// that cannot succeed no matter how many times it's retried today).
func (b *Budget) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	if b.used > 0 {
		b.used--
		_ = b.save() // a lost refund only errs on the cautious side
	}
}

func (b *Budget) Today() (used, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.used, b.capFn()
}

type budgeted struct {
	inner FlightLookup
	b     *Budget
}

// NewBudgeted refuses upstream calls once the day's budget is spent. Put
// it inside the cache so cache hits never consume budget.
func NewBudgeted(inner FlightLookup, b *Budget) Source { return &budgeted{inner: inner, b: b} }

func (l *budgeted) Status(ctx context.Context, q Query) (Status, bool, error) {
	f, ok, err := l.Flight(ctx, q)
	return statusOf(f, ok, err)
}

// Flight charges one unit per upstream call whichever method asked.
func (l *budgeted) Flight(ctx context.Context, q Query) (Flight, bool, error) {
	if !l.b.Allow() {
		l.b.refused(q.Callsign)
		return Flight{}, false, ErrBudget
	}
	f, ok, err := l.inner.Flight(ctx, q)
	if errors.Is(err, ErrAuth) {
		// A bad key was never going to succeed; don't let it count against
		// today's cap alongside the calls that could have.
		l.b.Refund()
	}
	return f, ok, err
}
