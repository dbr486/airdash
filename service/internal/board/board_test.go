package board

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"airdash/internal/flightstatus"
)

type fakeFetcher struct {
	mu    sync.Mutex
	calls []string
	dep   []Row
	arr   []Row
	errOn string // "dep" or "arr" fails
}

func (f *fakeFetcher) Fetch(ctx context.Context, airport string, arrivals bool) ([]Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir := "dep"
	if arrivals {
		dir = "arr"
	}
	f.calls = append(f.calls, dir+":"+airport)
	if f.errOn == dir {
		return nil, errors.New("boom")
	}
	if arrivals {
		return f.arr, nil
	}
	return f.dep, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRefresh_TwoCallsAndStatus(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	now := at(13, 30)
	b := New(f, 900, 3*time.Hour, func() time.Time { return now }, quiet())
	if err := b.Refresh(context.Background(), "AGP"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0] != "dep:AGP" || f.calls[1] != "arr:AGP" {
		t.Errorf("calls = %v", f.calls)
	}
	st, ok, err := b.Status(context.Background(), flightstatus.Query{Callsign: "WMT6026", AirlineIATA: "W4", Origin: "AGP", Dest: "FCO"})
	if err != nil || !ok || st.Kind != flightstatus.KindLate || st.DelayMin != 9 {
		t.Errorf("status = %+v ok=%v err=%v", st, ok, err)
	}
	if _, ok, _ := b.Status(context.Background(), flightstatus.Query{Callsign: "NOPE1", AirlineIATA: "ZZ", Origin: "AGP", Dest: "XXX"}); ok {
		t.Error("unmatched must be a miss, not an error")
	}
	s := b.Stats()
	if s.Airport != "AGP" || s.Flights != 24 || s.AgeS != 0 || s.CallsMonth != 2 || s.CallsCap != 900 {
		t.Errorf("stats = %+v", s)
	}
	now = now.Add(12 * time.Minute)
	if b.Stats().AgeS != 720 {
		t.Errorf("age = %d", b.Stats().AgeS)
	}
}

func TestStatus_BeforeAnyRefreshIsAMiss(t *testing.T) {
	b := New(&fakeFetcher{}, 900, 3*time.Hour, time.Now, quiet())
	if _, ok, err := b.Status(context.Background(), flightstatus.Query{Callsign: "WMT6026"}); ok || err != nil {
		t.Errorf("ok=%v err=%v", ok, err)
	}
	if s := b.Stats(); s.AgeS != -1 || s.Flights != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestRefresh_PartialFailureKeepsOldBoard(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	b := New(f, 900, 3*time.Hour, func() time.Time { return at(13, 30) }, quiet())
	_ = b.Refresh(context.Background(), "AGP")
	f.errOn = "arr"
	if err := b.Refresh(context.Background(), "AGP"); err == nil {
		t.Fatal("expected an error")
	}
	if s := b.Stats(); s.Flights != 24 || s.CallsMonth != 4 {
		t.Errorf("old board must survive, calls still counted: %+v", s)
	}
}

func TestRefresh_RefusedWhenCapSpent(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	b := New(f, 3, 3*time.Hour, func() time.Time { return at(13, 30) }, quiet())
	_ = b.Refresh(context.Background(), "AGP") // 2 calls
	if err := b.Refresh(context.Background(), "AGP"); !errors.Is(err, ErrCap) {
		t.Errorf("a refresh needing 2 calls with 1 left must be refused: %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("no upstream call when refused: %v", f.calls)
	}
}

func TestCounter_RollsOverAtMonth(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	now := time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	b := New(f, 4, 3*time.Hour, func() time.Time { return now }, quiet())
	_ = b.Refresh(context.Background(), "AGP")
	_ = b.Refresh(context.Background(), "AGP")
	if err := b.Refresh(context.Background(), "AGP"); !errors.Is(err, ErrCap) {
		t.Fatalf("cap: %v", err)
	}
	now = time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC)
	if err := b.Refresh(context.Background(), "AGP"); err != nil {
		t.Errorf("new month must reset: %v", err)
	}
	if s := b.Stats(); s.CallsMonth != 2 {
		t.Errorf("calls this month = %d", s.CallsMonth)
	}
}

func TestRun_RefreshesOnStartIntervalAndAirportChange(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	var mu sync.Mutex
	now := at(13, 30)
	airport := "AGP"
	b := New(f, 900, 3*time.Hour, func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		b.run(ctx, func() string { mu.Lock(); defer mu.Unlock(); return airport }, 90*time.Minute, tick)
		close(done)
	}()
	waitCalls := func(n int) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if f.count() >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("waiting for %d calls, have %d", n, f.count())
	}
	waitCalls(2) // immediate refresh
	tick <- now.Add(30 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if f.count() != 2 {
		t.Fatalf("no refresh before the interval: %v", f.calls)
	}
	mu.Lock()
	now = now.Add(91 * time.Minute)
	mu.Unlock()
	tick <- now
	waitCalls(4)
	mu.Lock()
	airport = "LEMG"
	mu.Unlock()
	tick <- now.Add(time.Minute)
	waitCalls(6)
	f.mu.Lock()
	last := f.calls[5]
	f.mu.Unlock()
	if last != "arr:LEMG" {
		t.Errorf("airport change must refetch: %v", f.calls)
	}
	mu.Lock()
	airport = ""
	mu.Unlock()
	tick <- now.Add(2 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && b.Stats().Flights != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s := b.Stats(); s.Flights != 0 || s.Airport != "" {
		t.Errorf("empty airport must clear the board: %+v", s)
	}
	cancel()
	<-done
}

func TestChain(t *testing.T) {
	hit := flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 7}
	first := lookupFunc(func(q flightstatus.Query) (flightstatus.Status, bool, error) {
		if q.Callsign == "HIT" {
			return hit, true, nil
		}
		return flightstatus.Status{}, false, nil
	})
	var secondCalls int
	second := lookupFunc(func(q flightstatus.Query) (flightstatus.Status, bool, error) {
		secondCalls++
		return flightstatus.Status{Kind: flightstatus.KindOnTime}, true, nil
	})
	c := Chain(first, second)
	if st, ok, _ := c.Status(context.Background(), flightstatus.Query{Callsign: "HIT"}); !ok || st != hit || secondCalls != 0 {
		t.Errorf("first must win: %+v %v calls=%d", st, ok, secondCalls)
	}
	if st, ok, _ := c.Status(context.Background(), flightstatus.Query{Callsign: "MISS"}); !ok || st.Kind != flightstatus.KindOnTime || secondCalls != 1 {
		t.Errorf("miss must fall through: %+v %v calls=%d", st, ok, secondCalls)
	}
	only := Chain(first, nil)
	if _, ok, err := only.Status(context.Background(), flightstatus.Query{Callsign: "MISS"}); ok || err != nil {
		t.Errorf("nil second: ok=%v err=%v", ok, err)
	}
}

type lookupFunc func(q flightstatus.Query) (flightstatus.Status, bool, error)

func (f lookupFunc) Status(ctx context.Context, q flightstatus.Query) (flightstatus.Status, bool, error) {
	return f(q)
}

func TestRun_BacksOffAfterErrorsAndGoesQuietOnCap(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t), errOn: "dep"}
	var mu sync.Mutex
	now := at(13, 30)
	b := New(f, 900, 2*time.Hour, func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { b.run(ctx, func() string { return "AGP" }, 90*time.Minute, tick); close(done) }()
	waitFor := func(cond func() bool, what string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("waiting for %s (calls=%d)", what, f.count())
	}
	waitFor(func() bool { return f.count() == 1 }, "first attempt")
	// Ten one-minute ticks after a failure: backoff (5, 10, 20 min...) allows
	// at most two more attempts, not ten.
	for i := 1; i <= 10; i++ {
		mu.Lock()
		now = now.Add(time.Minute)
		tick <- now
		mu.Unlock()
		time.Sleep(3 * time.Millisecond)
	}
	if c := f.count(); c > 3 {
		t.Errorf("a failing board must back off, got %d attempts in 10 minutes", c)
	}
	cancel()
	<-done

	// Cap spent: the loop must not hammer reserve every minute forever.
	f2 := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	b2 := New(f2, 2, 2*time.Hour, func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, quiet())
	ctx2, cancel2 := context.WithCancel(context.Background())
	tick2 := make(chan time.Time)
	done2 := make(chan struct{})
	go func() { b2.run(ctx2, func() string { return "AGP" }, time.Minute, tick2); close(done2) }()
	waitFor(func() bool { return f2.count() == 2 }, "b2 first refresh")
	for i := 0; i < 5; i++ {
		mu.Lock()
		now = now.Add(time.Minute)
		tick2 <- now
		mu.Unlock()
		time.Sleep(3 * time.Millisecond)
	}
	if f2.count() != 2 || b2.Stats().CallsMonth != 2 {
		t.Errorf("cap spent: no more attempts, got calls=%d stats=%+v", f2.count(), b2.Stats())
	}
	cancel2()
	<-done2
}

func TestStatus_StaleBoardIsAMiss(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	now := at(13, 30)
	b := New(f, 900, 3*time.Hour, func() time.Time { return now }, quiet())
	_ = b.Refresh(context.Background(), "AGP")
	q := flightstatus.Query{Callsign: "WMT6026", AirlineIATA: "W4", Origin: "AGP", Dest: "FCO"}
	if _, ok, _ := b.Status(context.Background(), q); !ok {
		t.Fatal("fresh board must answer")
	}
	now = now.Add(3*time.Hour + time.Minute)
	if _, ok, _ := b.Status(context.Background(), q); ok {
		t.Error("a board older than maxAge must be a miss so the paid fallback runs")
	}
}

func TestStatus_ICAOBoardAirportMatchesIATARoute(t *testing.T) {
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	b := New(f, 900, 3*time.Hour, func() time.Time { return at(13, 30) }, quiet())
	_ = b.Refresh(context.Background(), "LEMG")
	// The route lookup gives IATA codes; the board was configured as ICAO.
	st, ok, _ := b.Status(context.Background(), flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "ARN"})
	if !ok || st.Kind != flightstatus.KindEarly {
		t.Errorf("airline match must work with the airport's other code: ok=%v st=%+v", ok, st)
	}
}

func TestCounter_PersistsAcrossRestarts(t *testing.T) {
	path := t.TempDir() + "/board-calls.json"
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	now := at(13, 30)
	b := New(f, 900, 3*time.Hour, func() time.Time { return now }, quiet())
	if err := b.Persist(path); err != nil {
		t.Fatal(err)
	}
	_ = b.Refresh(context.Background(), "AGP")
	_ = b.Refresh(context.Background(), "AGP")
	again := New(f, 900, 3*time.Hour, func() time.Time { return now }, quiet())
	if err := again.Persist(path); err != nil {
		t.Fatal(err)
	}
	if s := again.Stats(); s.CallsMonth != 4 {
		t.Errorf("a restart must not forget this month's calls: %+v", s)
	}
	next := New(f, 900, 3*time.Hour, func() time.Time { return now.AddDate(0, 1, 0) }, quiet())
	_ = next.Persist(path)
	if s := next.Stats(); s.CallsMonth != 0 {
		t.Errorf("a new month starts at zero: %+v", s)
	}
}

func TestStatus_UsesTheSightingForDepartures(t *testing.T) {
	f := &fakeFetcher{dep: []Row{{FlightICAO: "RYR9ZZ", AirlineIATA: "FR", DepIATA: "AGP", DepICAO: "LEMG", ArrIATA: "STN", DepSched: at(14, 0), Status: "scheduled"}}}
	now := at(15, 30)
	b := New(f, 900, 3*time.Hour, func() time.Time { return now }, quiet())
	_ = b.Refresh(context.Background(), "AGP")
	st, ok, _ := b.Status(context.Background(), flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "STN", Seen: at(15, 20)})
	if !ok || st.Kind != flightstatus.KindLate || st.DelayMin != 70 {
		t.Errorf("seen 15:20 → departed ~15:10 → 70 min late: ok=%v st=%+v", ok, st)
	}
}
