package flightstatus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeLookup struct {
	calls int
	st    Status
	ok    bool
	err   error
}

func (f *fakeLookup) Status(ctx context.Context, q Query) (Status, bool, error) {
	f.calls++
	return f.st, f.ok, f.err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func quiet() *slog.Logger                { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCache_KnownAndUnknownTTLs(t *testing.T) {
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	inner := &fakeLookup{st: Status{Kind: KindLate, DelayMin: 7, Basis: "arrival"}, ok: true}
	c := NewCache(inner, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	for i := 0; i < 3; i++ {
		st, ok, err := c.Status(t.Context(), Query{Callsign: "TRA39Z"})
		if err != nil || !ok || st.DelayMin != 7 {
			t.Fatalf("call %d: %+v ok=%v err=%v", i, st, ok, err)
		}
	}
	if inner.calls != 1 {
		t.Errorf("inner calls = %d, want 1", inner.calls)
	}
	ck.advance(15*time.Minute + time.Second)
	_, _, _ = c.Status(t.Context(), Query{Callsign: "TRA39Z"})
	if inner.calls != 2 {
		t.Errorf("after TTL inner calls = %d, want 2", inner.calls)
	}
	unk := &fakeLookup{ok: false}
	c2 := NewCache(unk, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	_, _, _ = c2.Status(t.Context(), Query{Callsign: "ZZZ1"})
	_, _, _ = c2.Status(t.Context(), Query{Callsign: "ZZZ1"})
	if unk.calls != 1 {
		t.Errorf("unknown not cached: calls=%d", unk.calls)
	}
}

func TestCache_ErrorCooldown(t *testing.T) {
	ck := &clock{t: time.Now()}
	inner := &fakeLookup{err: errors.New("boom")}
	c := NewCache(inner, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	if _, _, err := c.Status(t.Context(), Query{Callsign: "A"}); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := c.Status(t.Context(), Query{Callsign: "A"}); !errors.Is(err, ErrCooldown) {
		t.Fatalf("expected ErrCooldown, got %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("cooldown must not call inner: calls=%d", inner.calls)
	}
	ck.advance(61 * time.Second)
	_, _, _ = c.Status(t.Context(), Query{Callsign: "A"})
	if inner.calls != 2 {
		t.Errorf("after cooldown calls=%d, want 2", inner.calls)
	}
}

// TestCache_AuthErrorLongerCooldown: a bad key does not fix itself, so
// retrying it every 60s (the normal error cooldown) just burns the daily
// budget on the same failure; ErrAuth gets a 1h cooldown instead.
func TestCache_AuthErrorLongerCooldown(t *testing.T) {
	ck := &clock{t: time.Now()}
	inner := &fakeLookup{err: ErrAuth}
	c := NewCache(inner, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	if _, _, err := c.Status(t.Context(), Query{Callsign: "A"}); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
	// Well past the normal 60s error cooldown, but still within the 1h
	// auth cooldown: must still be a cached cooldown, not a fresh call.
	ck.advance(5 * time.Minute)
	if _, _, err := c.Status(t.Context(), Query{Callsign: "A"}); !errors.Is(err, ErrCooldown) {
		t.Fatalf("auth cooldown should outlast the normal 60s cooldown: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("must not retry within the auth cooldown: calls=%d", inner.calls)
	}
	ck.advance(56 * time.Minute) // total 61 min: past the 1h auth cooldown
	_, _, _ = c.Status(t.Context(), Query{Callsign: "A"})
	if inner.calls != 2 {
		t.Errorf("after the 1h auth cooldown calls=%d, want 2", inner.calls)
	}
}

func TestBudget_CapAndRollover(t *testing.T) {
	ck := &clock{t: time.Date(2026, 9, 28, 23, 59, 0, 0, time.UTC)}
	limit := 3
	b := NewBudget(func() int { return limit }, ck.now, quiet())
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if b.Allow() {
		t.Fatal("4th call must be refused")
	}
	if used, c := b.Today(); used != 3 || c != 3 {
		t.Errorf("Today = %d/%d", used, c)
	}
	ck.advance(2 * time.Minute) // past midnight UTC
	if !b.Allow() {
		t.Fatal("new day must reset the counter")
	}
	if used, _ := b.Today(); used != 1 {
		t.Errorf("used after rollover = %d, want 1", used)
	}
	limit = 0
	if b.Allow() {
		t.Error("cap 0 must refuse")
	}
}

func TestBudget_Refund(t *testing.T) {
	ck := &clock{t: time.Now()}
	b := NewBudget(func() int { return 3 }, ck.now, quiet())
	b.Allow()
	b.Allow()
	b.Refund()
	if used, _ := b.Today(); used != 1 {
		t.Errorf("used after refund = %d, want 1", used)
	}
	b.Refund()
	b.Refund() // must not go negative
	if used, _ := b.Today(); used != 0 {
		t.Errorf("used after over-refund = %d, want 0 (floor at zero)", used)
	}
}

// TestBudgeted_RefundsOnAuthError: a bad key must not burn the daily
// budget on every tick, since it can never succeed until the key is fixed.
func TestBudgeted_RefundsOnAuthError(t *testing.T) {
	ck := &clock{t: time.Now()}
	b := NewBudget(func() int { return 5 }, ck.now, quiet())
	inner := &fakeLookup{err: ErrAuth}
	bl := NewBudgeted(inner, b)
	if _, _, err := bl.Status(t.Context(), Query{Callsign: "A"}); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
	if used, _ := b.Today(); used != 0 {
		t.Errorf("ErrAuth must refund the unit: used=%d, want 0", used)
	}
}

func TestCached_HitDoesNotConsumeBudget(t *testing.T) {
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	b := NewBudget(func() int { return 1 }, ck.now, quiet())
	inner := &fakeLookup{st: Status{Kind: KindOnTime}, ok: true}
	c := NewCache(NewBudgeted(inner, b), 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	for i := 0; i < 5; i++ {
		if _, ok, err := c.Status(t.Context(), Query{Callsign: "TRA39Z"}); err != nil || !ok {
			t.Fatalf("call %d: ok=%v err=%v", i, ok, err)
		}
	}
	if used, _ := b.Today(); used != 1 || inner.calls != 1 {
		t.Errorf("used=%d inner=%d, want 1/1", used, inner.calls)
	}
	// A second callsign exceeds the cap of 1: ErrBudget, cached as a cooldown.
	if _, _, err := c.Status(t.Context(), Query{Callsign: "OTHER1"}); !errors.Is(err, ErrBudget) {
		t.Errorf("expected ErrBudget, got %v", err)
	}
	if _, _, err := c.Status(t.Context(), Query{Callsign: "OTHER1"}); !errors.Is(err, ErrCooldown) {
		t.Errorf("budget refusal should enter cooldown, got %v", err)
	}
}
