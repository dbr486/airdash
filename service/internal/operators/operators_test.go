package operators

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"airdash/internal/routes"
	"airdash/internal/standing"

	"airdash/internal/usage"
)

func serveFixture(t *testing.T, status int, file string, gotUA, gotPath *string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotUA != nil {
			*gotUA = r.Header.Get("User-Agent")
		}
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestADSBDBOwner_FlagCode(t *testing.T) {
	var ua, path string
	srv := serveFixture(t, 200, "adsbdb_aircraft_3451d5.json", &ua, &path)
	defer srv.Close()
	code, ok, err := NewADSBDBOwner(srv.Client(), srv.URL, "airdash-test").FlagCode(context.Background(), "3451d5")
	if err != nil || !ok || code != "VLG" {
		t.Fatalf("got (%q,%v,%v)", code, ok, err)
	}
	if path != "/v0/aircraft/3451d5" || ua != "airdash-test" {
		t.Errorf("path=%q ua=%q", path, ua)
	}
}

func TestADSBDBOwner_Unknown(t *testing.T) {
	for _, status := range []int{404, 200} {
		srv := serveFixture(t, status, "adsbdb_aircraft_unknown.json", nil, nil)
		code, ok, err := NewADSBDBOwner(srv.Client(), srv.URL, "ua").FlagCode(context.Background(), "ffffff")
		srv.Close()
		if err != nil || ok || code != "" {
			t.Errorf("status %d: got (%q,%v,%v)", status, code, ok, err)
		}
	}
}

func TestADSBDBOwner_Error(t *testing.T) {
	srv := serveFixture(t, 500, "adsbdb_aircraft_unknown.json", nil, nil)
	defer srv.Close()
	if _, _, err := NewADSBDBOwner(srv.Client(), srv.URL, "ua").FlagCode(context.Background(), "3451d5"); err == nil {
		t.Fatal("expected error")
	}
}

type fakeFlags struct {
	code  string
	ok    bool
	err   error
	calls int
}

func (f *fakeFlags) FlagCode(context.Context, string) (string, bool, error) {
	f.calls++
	return f.code, f.ok, f.err
}

func TestCache_TTLs(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	ctx := context.Background()

	known := &fakeFlags{code: "VLG", ok: true}
	c := NewCache(known, 7*24*time.Hour, 24*time.Hour, 60*time.Second, clock)
	_, _, _ = c.FlagCode(ctx, "3451D5")
	now = now.Add(7*24*time.Hour - time.Second)
	_, _, _ = c.FlagCode(ctx, "3451d5") // key is case-insensitive
	if known.calls != 1 {
		t.Errorf("known: calls=%d within TTL", known.calls)
	}
	now = now.Add(2 * time.Second)
	_, _, _ = c.FlagCode(ctx, "3451d5")
	if known.calls != 2 {
		t.Errorf("known: calls=%d after TTL", known.calls)
	}

	unk := &fakeFlags{}
	c = NewCache(unk, 7*24*time.Hour, 24*time.Hour, 60*time.Second, clock)
	_, _, _ = c.FlagCode(ctx, "aaaaaa")
	now = now.Add(24*time.Hour - time.Second)
	_, _, _ = c.FlagCode(ctx, "aaaaaa")
	if unk.calls != 1 {
		t.Errorf("unknown: calls=%d within TTL", unk.calls)
	}
	now = now.Add(2 * time.Second)
	_, _, _ = c.FlagCode(ctx, "aaaaaa")
	if unk.calls != 2 {
		t.Errorf("unknown: calls=%d after TTL", unk.calls)
	}

	bad := &fakeFlags{err: errors.New("boom")}
	c = NewCache(bad, 7*24*time.Hour, 24*time.Hour, 60*time.Second, clock)
	if _, _, err := c.FlagCode(ctx, "bbbbbb"); err == nil || errors.Is(err, ErrCooldown) {
		t.Fatalf("first call err=%v", err)
	}
	now = now.Add(59 * time.Second)
	if _, _, err := c.FlagCode(ctx, "bbbbbb"); !errors.Is(err, ErrCooldown) {
		t.Fatalf("want ErrCooldown, got %v", err)
	}
	if bad.calls != 1 {
		t.Errorf("inner called during cooldown: %d", bad.calls)
	}
	now = now.Add(2 * time.Second)
	_, _, _ = c.FlagCode(ctx, "bbbbbb")
	if bad.calls != 2 {
		t.Errorf("not retried after cooldown: %d", bad.calls)
	}
}

func TestResolver_PrefixWins(t *testing.T) {
	f := &fakeFlags{}
	a, ok, err := NewResolver(f).Operator(context.Background(), "SCR137", "3cc086", true)
	if err != nil || !ok || a != (standing.Airline{Name: "Silver Cloud Air"}) {
		t.Fatalf("got %+v %v %v", a, ok, err)
	}
	if f.calls != 0 {
		t.Error("owner lookup should not run")
	}
}

func TestResolver_OwnerWhenNoCallsign(t *testing.T) {
	f := &fakeFlags{code: "VLG", ok: true}
	a, ok, err := NewResolver(f).Operator(context.Background(), "", "3451d5", false)
	if err != nil || !ok || a != (standing.Airline{Name: "Vueling Airlines", IATA: "VY"}) {
		t.Fatalf("got %+v %v %v", a, ok, err)
	}
}

func TestResolver_OwnerFlagUnknownToVRS(t *testing.T) {
	for _, f := range []*fakeFlags{{code: "QQQ", ok: true}, {}} {
		a, ok, err := NewResolver(f).Operator(context.Background(), "", "3451d5", false)
		if err != nil || ok || a != (standing.Airline{}) {
			t.Errorf("got %+v %v %v", a, ok, err)
		}
	}
}

func TestResolver_SkipsOwnerWhenAirlineKnown(t *testing.T) {
	f := &fakeFlags{code: "VLG", ok: true}
	a, ok, err := NewResolver(f).Operator(context.Background(), "XQZ12", "abc123", true)
	if err != nil || ok || a != (standing.Airline{}) || f.calls != 0 {
		t.Fatalf("got %+v %v %v calls=%d", a, ok, err, f.calls)
	}
}

func TestResolver_SkipsNonICAOHex(t *testing.T) {
	f := &fakeFlags{code: "VLG", ok: true}
	for _, hex := range []string{"~1a2b3c", ""} {
		a, ok, err := NewResolver(f).Operator(context.Background(), "", hex, false)
		if err != nil || ok || a != (standing.Airline{}) {
			t.Errorf("hex %q: got %+v %v %v", hex, a, ok, err)
		}
	}
	if f.calls != 0 {
		t.Error("lookup should not run")
	}
}

func TestResolver_PropagatesError(t *testing.T) {
	want := errors.New("boom")
	_, _, err := NewResolver(&fakeFlags{err: want}).Operator(context.Background(), "", "3451d5", false)
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

func TestCache_DoneContextSkipsInner(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	inner := &fakeFlags{code: "VLG", ok: true}
	c := NewCache(inner, time.Hour, time.Hour, time.Minute, clock)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.FlagCode(dead, "3451d5"); err == nil || inner.calls != 0 {
		t.Fatalf("err=%v calls=%d, want error and no inner call", err, inner.calls)
	}
	// No cooldown may have been stored by the skipped call.
	code, ok, err := c.FlagCode(context.Background(), "3451d5")
	if err != nil || !ok || code != "VLG" || inner.calls != 1 {
		t.Fatalf("live retry: %q %v %v calls=%d", code, ok, err, inner.calls)
	}
	// A cached answer is still served on a spent budget.
	code, ok, err = c.FlagCode(dead, "3451d5")
	if err != nil || !ok || code != "VLG" || inner.calls != 1 {
		t.Errorf("cache hit on dead ctx: %q %v %v calls=%d", code, ok, err, inner.calls)
	}
}

// A closed adsbdb gate made no upstream call, so the hex must reach inner
// again the moment the gate reopens rather than sit out a cooldown.
func TestCache_GateRejectionStartsNoCooldown(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	inner := &fakeFlags{err: routes.ErrRateLimited}
	c := NewCache(inner, 7*24*time.Hour, 24*time.Hour, 60*time.Second, clock)

	if _, _, err := c.FlagCode(t.Context(), "3451d5"); !errors.Is(err, routes.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	inner.err, inner.code, inner.ok = nil, "RYR", true
	if code, ok, err := c.FlagCode(t.Context(), "3451d5"); err != nil || !ok || code != "RYR" {
		t.Fatalf("second call: code=%q ok=%v err=%v", code, ok, err)
	}
}

func TestCache_CapRefusalStartsNoCooldown(t *testing.T) {
	now := time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)
	inner := &fakeFlags{err: usage.ErrCapReached}
	c := NewCache(inner, 7*24*time.Hour, 24*time.Hour, 60*time.Second, func() time.Time { return now })
	if _, _, err := c.FlagCode(t.Context(), "3451d5"); !errors.Is(err, usage.ErrCapReached) {
		t.Fatalf("err = %v", err)
	}
	inner.err, inner.code, inner.ok = nil, "RYR", true
	if code, ok, err := c.FlagCode(t.Context(), "3451d5"); err != nil || !ok || code != "RYR" {
		t.Fatalf("second call: code=%q ok=%v err=%v", code, ok, err)
	}
}
