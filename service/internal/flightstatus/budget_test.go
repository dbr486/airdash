package flightstatus

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestBudgetPersistRestoresSameDay(t *testing.T) {
	path := t.TempDir() + "/aeroapi-budget.json"
	ck := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	b := NewBudget(func() int { return 65 }, ck.now, quiet())
	if err := b.Persist(path); err != nil {
		t.Fatal(err)
	}
	b.Allow()
	b.Allow()
	again := NewBudget(func() int { return 65 }, ck.now, quiet())
	if err := again.Persist(path); err != nil {
		t.Fatal(err)
	}
	if used, _ := again.Today(); used != 2 {
		t.Fatalf("a restart must keep today's count: used = %d", used)
	}
}

func TestBudgetPersistIgnoresOtherDay(t *testing.T) {
	path := t.TempDir() + "/aeroapi-budget.json"
	ck := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	b := NewBudget(func() int { return 65 }, ck.now, quiet())
	_ = b.Persist(path)
	b.Allow()
	ck.advance(24 * time.Hour)
	again := NewBudget(func() int { return 65 }, ck.now, quiet())
	_ = again.Persist(path)
	if used, _ := again.Today(); used != 0 {
		t.Fatalf("yesterday's count leaked into today: %d", used)
	}
}

func TestBudgetCapHitAt(t *testing.T) {
	hit := time.Date(2026, 11, 2, 9, 28, 0, 0, time.UTC)
	ck := &clock{t: hit}
	path := t.TempDir() + "/aeroapi-budget.json"
	b := NewBudget(func() int { return 2 }, ck.now, quiet())
	_ = b.Persist(path)
	b.Allow()
	b.Allow()
	if !b.CapHitAt().IsZero() {
		t.Fatal("cap not hit yet")
	}
	b.Allow()
	ck.advance(time.Minute)
	b.Allow()
	if !b.CapHitAt().Equal(hit) {
		t.Fatalf("CapHitAt = %v, want the first refusal %v", b.CapHitAt(), hit)
	}
	again := NewBudget(func() int { return 2 }, ck.now, quiet())
	_ = again.Persist(path)
	if !again.CapHitAt().Equal(hit) {
		t.Fatalf("cap hit time lost on restart: %v", again.CapHitAt())
	}
	ck.advance(24 * time.Hour)
	if !b.CapHitAt().IsZero() {
		t.Fatal("a new day must clear the cap hit time")
	}
}

func TestBudgetedOnRefuseGetsCallsign(t *testing.T) {
	b := NewBudget(func() int { return 0 }, time.Now, quiet())
	var got []string
	b.OnRefuse(func(s string) { got = append(got, s) })
	l := NewBudgeted(&fakeLookup{}, b)
	_, _, err := l.Status(context.Background(), Query{Callsign: "RYR1"})
	if err != ErrBudget || len(got) != 1 || got[0] != "RYR1" {
		t.Fatalf("err=%v refused=%v", err, got)
	}
}

// A refusal must not be cached: asking the budget again is free, and a
// cached refusal later surfaces as ErrCooldown, i.e. a false "error".
func TestCacheDoesNotCacheBudgetRefusal(t *testing.T) {
	ck := &clock{t: time.Now()}
	f := &fakeLookup{err: ErrBudget}
	c := NewCache(f, 15*time.Minute, 15*time.Minute, time.Minute, ck.now)
	q := Query{Callsign: "RYR1"}
	if _, _, err := c.Flight(context.Background(), q); err != ErrBudget {
		t.Fatalf("first err = %v", err)
	}
	f.err, f.ok = nil, true
	if _, ok, err := c.Flight(context.Background(), q); err != nil || !ok || f.calls != 2 {
		t.Fatalf("second lookup: ok=%v err=%v calls=%d", ok, err, f.calls)
	}
}

// A count that cannot be saved could be spent again after a restart, so an
// unsaveable call is refused rather than made.
func TestBudgetRefusesWhenCountCannotBeSaved(t *testing.T) {
	dir := t.TempDir()
	ck := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	b := NewBudget(func() int { return 65 }, ck.now, quiet())
	if err := b.Persist(dir + "/aeroapi-budget.json"); err != nil {
		t.Fatal(err)
	}
	if !b.Allow() {
		t.Fatal("first call should be allowed")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if b.Allow() {
		t.Fatal("a call whose count cannot be saved must be refused")
	}
	if used, _ := b.Today(); used != 1 {
		t.Fatalf("used = %d, want 1 (the refused call not counted)", used)
	}
}

// An unreadable state file may hide today's spend: treat today as spent
// (Persist still errors so the caller warns); tomorrow starts fresh.
func TestBudgetCorruptStateFailsClosedForTheDay(t *testing.T) {
	path := t.TempDir() + "/aeroapi-budget.json"
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	ck := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	b := NewBudget(func() int { return 65 }, ck.now, quiet())
	if err := b.Persist(path); err == nil {
		t.Fatal("corrupt state must be reported")
	}
	if b.Allow() {
		t.Fatal("corrupt state must not allow paid calls today")
	}
	ck.advance(24 * time.Hour)
	if !b.Allow() {
		t.Fatal("a new day must start fresh")
	}
}

func TestBudgetInvalidStateFailsClosed(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"day":"2026-11-02"}`, `{"day":"2026-11-02","used":-3}`, `{"day":"yesterday","used":1}`} {
		path := t.TempDir() + "/aeroapi-budget.json"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		ck := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
		b := NewBudget(func() int { return 65 }, ck.now, quiet())
		if err := b.Persist(path); err == nil {
			t.Errorf("%s: invalid state must be reported", body)
		}
		if b.Allow() {
			t.Errorf("%s: invalid state must not allow paid calls today", body)
		}
		if b.Blocked() == "" {
			t.Errorf("%s: the block must be visible", body)
		}
	}
}

func TestBudgetBlockedReportsSaveFailureAndClears(t *testing.T) {
	dir := t.TempDir()
	ck := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	b := NewBudget(func() int { return 65 }, ck.now, quiet())
	_ = b.Persist(dir + "/aeroapi-budget.json")
	if b.Blocked() != "" {
		t.Fatal("healthy budget reports a block")
	}
	os.Chmod(dir, 0o500)
	b.Allow()
	if b.Blocked() == "" {
		t.Fatal("a save failure must be visible")
	}
	os.Chmod(dir, 0o700)
	if !b.Allow() || b.Blocked() != "" {
		t.Fatalf("a successful save must clear the block: %q", b.Blocked())
	}
}
