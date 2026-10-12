package flightstatus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

var ErrBudget = errors.New("flightstatus: daily lookup budget exhausted")

// Budget caps upstream AeroAPI calls per UTC day so the Personal tier's
// free credit is never exceeded by accident. In memory only: a restart
// starts the day's count again, which errs on the generous side.
type Budget struct {
	capFn  func() int
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	day      string
	used     int
	warnedOn string
}

func NewBudget(capFn func() int, now func() time.Time, logger *slog.Logger) *Budget {
	if logger == nil {
		logger = slog.Default()
	}
	return &Budget{capFn: capFn, now: now, logger: logger}
}

func (b *Budget) roll() {
	d := b.now().UTC().Format("2006-01-02")
	if d != b.day {
		b.day, b.used = d, 0
	}
}

func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	limit := b.capFn()
	if b.used >= limit {
		if b.warnedOn != b.day && limit > 0 {
			b.warnedOn = b.day
			b.logger.Warn("delay budget exhausted", "cap", limit, "day", b.day)
		}
		return false
	}
	b.used++
	return true
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
	}
}

func (b *Budget) Today() (used, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.used, b.capFn()
}

type budgeted struct {
	inner Lookup
	b     *Budget
}

// NewBudgeted refuses upstream calls once the day's budget is spent. Put
// it inside the cache so cache hits never consume budget.
func NewBudgeted(inner Lookup, b *Budget) Lookup { return &budgeted{inner: inner, b: b} }

func (l *budgeted) Status(ctx context.Context, q Query) (Status, bool, error) {
	if !l.b.Allow() {
		return Status{}, false, ErrBudget
	}
	st, ok, err := l.inner.Status(ctx, q)
	if errors.Is(err, ErrAuth) {
		// A bad key was never going to succeed; don't let it count against
		// today's cap alongside the calls that could have.
		l.b.Refund()
	}
	return st, ok, err
}
