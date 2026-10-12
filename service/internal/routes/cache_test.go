package routes

import (
	"context"
	"errors"
	"testing"
	"time"
)

const testCooldown = 30 * time.Second

type fakeLookup struct {
	calls int
	route Route
	ok    bool
	err   error
}

func (f *fakeLookup) Route(ctx context.Context, callsign string) (Route, bool, error) {
	f.calls++
	return f.route, f.ok, f.err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestCache_KnownRouteIsReusedUntilTTL(t *testing.T) {
	inner := &fakeLookup{route: Route{OriginIATA: "LHR"}, ok: true}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	c := NewCache(inner, 6*time.Hour, time.Hour, testCooldown, ck.now)

	for i := 0; i < 3; i++ {
		r, ok, err := c.Route(t.Context(), "BAW466")
		if err != nil || !ok || r.OriginIATA != "LHR" {
			t.Fatalf("call %d: r=%+v ok=%v err=%v", i, r, ok, err)
		}
	}
	if inner.calls != 1 {
		t.Errorf("inner called %d times, want 1", inner.calls)
	}

	ck.advance(6*time.Hour + time.Second)
	_, _, _ = c.Route(t.Context(), "BAW466")
	if inner.calls != 2 {
		t.Errorf("after TTL inner called %d times, want 2", inner.calls)
	}
}

func TestCache_UnknownIsCachedForShorterTTL(t *testing.T) {
	inner := &fakeLookup{ok: false}
	ck := &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	c := NewCache(inner, 6*time.Hour, time.Hour, testCooldown, ck.now)

	_, ok, _ := c.Route(t.Context(), "ZZZ999")
	_, ok2, _ := c.Route(t.Context(), "ZZZ999")
	if ok || ok2 || inner.calls != 1 {
		t.Fatalf("unknown should be cached: ok=%v ok2=%v calls=%d", ok, ok2, inner.calls)
	}
	ck.advance(time.Hour + time.Second)
	_, _, _ = c.Route(t.Context(), "ZZZ999")
	if inner.calls != 2 {
		t.Errorf("after unknown TTL inner called %d times, want 2", inner.calls)
	}
}

func TestCache_ErrorsAreNotCached(t *testing.T) {
	inner := &fakeLookup{err: errors.New("boom")}
	ck := &clock{t: time.Now()}
	c := NewCache(inner, 6*time.Hour, time.Hour, testCooldown, ck.now)

	// First call: inner errors, callsign enters cooldown.
	_, _, err1 := c.Route(t.Context(), "BAW466")
	if err1 == nil {
		t.Fatal("expected error to propagate")
	}
	if inner.calls != 1 {
		t.Fatalf("inner called %d times, want 1", inner.calls)
	}

	// Second call, still within the cooldown: must not call inner again.
	_, _, err2 := c.Route(t.Context(), "BAW466")
	if !errors.Is(err2, ErrCooldown) {
		t.Fatalf("err2 = %v, want ErrCooldown", err2)
	}
	if inner.calls != 1 {
		t.Errorf("inner called during cooldown: calls=%d, want 1", inner.calls)
	}

	// Past the cooldown: inner is called again.
	ck.advance(testCooldown + time.Second)
	_, _, err3 := c.Route(t.Context(), "BAW466")
	if err3 == nil {
		t.Fatal("expected error to propagate after cooldown")
	}
	if inner.calls != 2 {
		t.Errorf("inner called %d times after cooldown, want 2", inner.calls)
	}
}

func TestCache_KeysAreIndependent(t *testing.T) {
	inner := &fakeLookup{ok: true}
	ck := &clock{t: time.Now()}
	c := NewCache(inner, time.Hour, time.Hour, testCooldown, ck.now)
	_, _, _ = c.Route(t.Context(), "A")
	_, _, _ = c.Route(t.Context(), "B")
	if inner.calls != 2 {
		t.Errorf("calls=%d, want 2", inner.calls)
	}
}
