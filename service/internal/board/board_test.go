package board

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	err   error  // what the failing direction returns; nil is "boom"
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
		if f.err != nil {
			return nil, f.err
		}
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
		b.run(ctx, func() string { mu.Lock(); defer mu.Unlock(); return airport }, every(90*time.Minute), tick)
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
	go func() { b.run(ctx, func() string { return "AGP" }, every(90*time.Minute), tick); close(done) }()
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
	go func() { b2.run(ctx2, func() string { return "AGP" }, every(time.Minute), tick2); close(done2) }()
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

func routeBoard(t *testing.T, rows []Row, now *time.Time) *Board {
	t.Helper()
	b := New(&fakeFetcher{dep: rows}, 900, 3*time.Hour, func() time.Time { return *now }, quiet())
	if err := b.Refresh(context.Background(), "AGP"); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoute_UniqueCallsignGivesBothEnds(t *testing.T) {
	now := at(13, 30)
	b := routeBoard(t, []Row{{FlightICAO: "ABC123", DepIATA: "AGP", DepICAO: "LEMG", ArrIATA: "FCO", ArrICAO: "LIRF", DepSched: at(13, 20)}}, &now)
	got, ok := b.Route(flightstatus.Query{Callsign: " abc123 "}, now)
	want := flightstatus.FlightRoute{OriginIATA: "AGP", OriginICAO: "LEMG", DestIATA: "FCO", DestICAO: "LIRF"}
	if !ok || got != want {
		t.Errorf("got %+v ok=%v", got, ok)
	}
}

func TestRoute_ArrivalRowUsesArrivalTime(t *testing.T) {
	now := at(13, 30)
	b := routeBoard(t, []Row{{FlightICAO: "ABC123", DepIATA: "JCU", DepICAO: "LTAF", ArrIATA: "AGP", ArrICAO: "LEMG", DepSched: at(9, 0), ArrSched: at(13, 40)}}, &now)
	if got, ok := b.Route(flightstatus.Query{Callsign: "ABC123"}, now); !ok || got.OriginIATA != "JCU" || got.DestICAO != "LEMG" {
		t.Errorf("got %+v ok=%v", got, ok)
	}
}

func TestRoute_YesterdaysFlightIsAMiss(t *testing.T) {
	now := at(13, 30)
	b := routeBoard(t, []Row{{FlightICAO: "ABC123", DepIATA: "AGP", ArrIATA: "FCO", DepSched: at(13, 20).Add(-20 * time.Hour)}}, &now)
	if _, ok := b.Route(flightstatus.Query{Callsign: "ABC123"}, now); ok {
		t.Error("a row 20 h away is another day's flight")
	}
}

func TestRoute_TwoPlausibleRowsFailClosed(t *testing.T) {
	now := at(13, 30)
	b := routeBoard(t, []Row{
		{FlightICAO: "ABC123", DepIATA: "AGP", ArrIATA: "FCO", DepSched: at(13, 0)},
		{FlightICAO: "ABC123", DepIATA: "AGP", ArrIATA: "MAD", DepSched: at(13, 25)},
	}, &now)
	if _, ok := b.Route(flightstatus.Query{Callsign: "ABC123"}, now); ok {
		t.Error("a tie must be a miss so the paid exact source can answer")
	}
}

func TestRoute_StaleBoardAndOtherCallsignAreMisses(t *testing.T) {
	now := at(13, 30)
	b := routeBoard(t, []Row{{FlightICAO: "ABC123", DepIATA: "AGP", ArrIATA: "FCO", DepSched: at(13, 20)}}, &now)
	if _, ok := b.Route(flightstatus.Query{Callsign: "XYZ9"}, now); ok {
		t.Error("non-matching callsign must miss")
	}
	if _, ok := b.Route(flightstatus.Query{}, now); ok {
		t.Error("no callsign must miss")
	}
	later := now.Add(3*time.Hour + time.Minute)
	if _, ok := b.Route(flightstatus.Query{Callsign: "ABC123"}, later); ok {
		t.Error("a board older than maxAge must miss")
	}
}

// The board's row must be the leg flying now: a callsign shared by legs of
// one tag-on flight, or a row that has already landed or was cancelled, is
// another flight instance and must not name the aircraft's route.
func TestRoute_OnlyTheLegAirborneNow(t *testing.T) {
	now := at(13, 30)
	cases := []struct {
		name string
		row  Row
		want bool
	}{
		{"departed 10 min ago", Row{DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(13, 15), DepActual: at(13, 20)}, true},
		{"no actual yet, 30 min behind schedule", Row{DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(13, 0)}, true},
		{"later leg, departs in 2 h", Row{DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(15, 30)}, false},
		{"departed 2 h ago", Row{DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(11, 25), DepActual: at(11, 30)}, false},
		{"arriving in 20 min", Row{DepIATA: "TFS", ArrIATA: "AGP", ArrSched: at(13, 50)}, true},
		{"arrival 4 h away (an earlier leg flies now)", Row{DepIATA: "TFS", ArrIATA: "AGP", ArrSched: at(17, 30)}, false},
		{"landed 30 min ago", Row{DepIATA: "TFS", ArrIATA: "AGP", ArrSched: at(13, 0), ArrActual: at(13, 0)}, false},
		{"later leg, schedule recent but estimate 3 h out", Row{DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(13, 0), DepEst: at(16, 30)}, false},
		{"short inbound leg not departed yet", Row{DepIATA: "MLN", ArrIATA: "AGP", DepEst: at(13, 50), ArrEst: at(14, 20)}, false},
		{"cancelled", Row{DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(13, 20), Status: "cancelled"}, false},
	}
	for _, c := range cases {
		r := c.row
		r.FlightICAO = "WZZ2765"
		b := routeBoard(t, []Row{r}, &now)
		if _, ok := b.Route(flightstatus.Query{Callsign: "WZZ2765"}, now); ok != c.want {
			t.Errorf("%s: ok=%v, want %v", c.name, ok, c.want)
		}
	}
}

func TestRoute_OtherLegDoesNotBlockTheAirborneOne(t *testing.T) {
	now := at(13, 30)
	b := routeBoard(t, []Row{
		{FlightICAO: "WZZ2765", DepIATA: "TFS", ArrIATA: "AGP", ArrSched: at(12, 40), ArrActual: at(12, 35)},
		{FlightICAO: "WZZ2765", DepIATA: "AGP", ArrIATA: "WAW", DepSched: at(13, 15), DepActual: at(13, 20)},
	}, &now)
	if got, ok := b.Route(flightstatus.Query{Callsign: "WZZ2765"}, now); !ok || got.DestIATA != "WAW" {
		t.Errorf("the landed inbound leg must not tie with the outbound: %+v ok=%v", got, ok)
	}
}

func TestBoardCapHitAtPersistsForMonth(t *testing.T) {
	path := t.TempDir() + "/board-calls.json"
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	now := at(13, 30)
	clock := func() time.Time { return now }
	b := New(f, 2, 3*time.Hour, clock, quiet())
	_ = b.Persist(path)
	var refused []string
	b.OnRefuse(func(s string) { refused = append(refused, s) })
	_ = b.Refresh(context.Background(), "AGP")
	if !b.CapHitAt().IsZero() {
		t.Fatal("cap not hit yet")
	}
	if err := b.Refresh(context.Background(), "AGP"); !errors.Is(err, ErrCap) {
		t.Fatalf("err = %v, want ErrCap", err)
	}
	hit := now
	now = now.Add(time.Hour)
	_ = b.Refresh(context.Background(), "AGP")
	if !b.CapHitAt().Equal(hit) || len(refused) != 2 || refused[0] != "AGP" {
		t.Fatalf("CapHitAt=%v refused=%v", b.CapHitAt(), refused)
	}
	again := New(f, 2, 3*time.Hour, clock, quiet())
	_ = again.Persist(path)
	if !again.CapHitAt().Equal(hit) {
		t.Fatalf("cap hit time lost on restart: %v", again.CapHitAt())
	}
	now = now.AddDate(0, 1, 0)
	if !b.CapHitAt().IsZero() {
		t.Fatal("a new month must clear the cap hit time")
	}
}

func TestBoardOnFailOnlyForAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: invalid api_key (unknown_api_key)", ErrAPI), 1},
		{errors.New("airlabs: status 503"), 0},
	} {
		f := &fakeFetcher{dep: deps(t), arr: arrs(t), errOn: "dep", err: tc.err}
		b := New(f, 900, 2*time.Hour, func() time.Time { return at(13, 30) }, quiet())
		var mu sync.Mutex
		var got []string
		b.OnFail(func(subject, msg string) { mu.Lock(); got = append(got, subject+"|"+msg); mu.Unlock() })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			b.run(ctx, func() string { return "AGP" }, every(90*time.Minute), make(chan time.Time))
			close(done)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for f.count() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
		<-done
		mu.Lock()
		if len(got) != tc.want || (tc.want == 1 && !strings.Contains(got[0], "AGP|")) {
			t.Errorf("err %v: hook calls = %v, want %d", tc.err, got, tc.want)
		}
		mu.Unlock()
	}
}

func every(d time.Duration) func(time.Time) time.Duration {
	return func(time.Time) time.Duration { return d }
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestIntervalSpreadsRemainingCalls(t *testing.T) {
	now := utc("2026-11-01T00:00:00Z")
	b := New(&fakeFetcher{}, 1, 3*time.Hour, func() time.Time { return now }, quiet())
	b.SetCapFunc(func() int { return 900 })
	if got := b.Interval(now); got != 96*time.Minute {
		t.Errorf("start of a 30-day month, 900 calls: interval = %v, want 1h36m", got)
	}
	b.mu.Lock()
	b.year, b.month, b.calls = 2026, time.November, 880
	b.mu.Unlock()
	now = utc("2026-11-30T00:00:00Z")
	if got := b.Interval(now); got != 144*time.Minute {
		t.Errorf("one day and 20 calls left: interval = %v, want 2h24m", got)
	}
}

func TestIntervalFloorAndCapReached(t *testing.T) {
	now := utc("2026-11-01T00:00:00Z")
	limit := 100000
	b := New(&fakeFetcher{}, 1, 3*time.Hour, func() time.Time { return now }, quiet())
	b.SetCapFunc(func() int { return limit })
	if got := b.Interval(now); got != 15*time.Minute {
		t.Errorf("huge cap: interval = %v, want the 15 min floor", got)
	}
	limit = 1
	if iv, reached := b.Schedule(); iv != 0 || !reached || b.Interval(now) != 0 {
		t.Errorf("cap 1 cannot fund a refresh: interval=%v reached=%v", iv, reached)
	}
	if s := b.Stats(); s.CallsCap != 1 {
		t.Errorf("stats must report the live cap: %+v", s)
	}
}

func TestLiveCapRaiseResumesRefresh(t *testing.T) {
	var mu sync.Mutex
	now := utc("2026-11-02T10:00:00Z")
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	b := New(f, 1, 3*time.Hour, clock, quiet())
	limit := 2
	var lmu sync.Mutex
	b.SetCapFunc(func() int { lmu.Lock(); defer lmu.Unlock(); return limit })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { b.run(ctx, func() string { return "AGP" }, b.Interval, tick); close(done) }()
	waitCount(t, f, 2)
	for i := 0; i < 3; i++ {
		mu.Lock()
		now = now.Add(time.Hour)
		mu.Unlock()
		tick <- now
	}
	if f.count() != 2 {
		t.Fatalf("cap spent: calls = %d, want 2", f.count())
	}
	// Raised far enough that the spread interval (15 min floor) has passed
	// since the last refresh three hours ago.
	lmu.Lock()
	limit = 100000
	lmu.Unlock()
	tick <- now
	waitCount(t, f, 4)
	cancel()
	<-done
}

func TestMonthRolloverResumes(t *testing.T) {
	var mu sync.Mutex
	now := utc("2026-11-30T10:00:00Z")
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
	b := New(f, 1, 3*time.Hour, clock, quiet())
	limit := 2
	var lmu sync.Mutex
	b.SetCapFunc(func() int { lmu.Lock(); defer lmu.Unlock(); return limit })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { b.run(ctx, func() string { return "AGP" }, b.Interval, tick); close(done) }()
	waitCount(t, f, 2)
	mu.Lock()
	now = utc("2026-11-30T23:59:00Z")
	mu.Unlock()
	tick <- now
	if f.count() != 2 {
		t.Fatalf("cap spent before the month ends: calls = %d", f.count())
	}
	// A new month with the usual cap: last refresh 14 h ago, far past the
	// ~99 min interval, so the next tick refreshes.
	lmu.Lock()
	limit = 900
	lmu.Unlock()
	mu.Lock()
	now = utc("2026-12-01T00:01:00Z")
	mu.Unlock()
	tick <- now
	waitCount(t, f, 4)
	cancel()
	<-done
}

func waitCount(t *testing.T, f *fakeFetcher, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.count() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.count() != n {
		t.Fatalf("fetch calls = %d, want %d", f.count(), n)
	}
}

// Coming out of a spent cap (a modest raise, or a new month with the same
// small cap) must refresh on the next tick, not wait a whole spread
// interval measured from the last refresh before the block.
func TestCapUnblockRefreshesAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raise int
		next  string
	}{
		{"modest raise", 4, "2026-11-02T13:00:00Z"},
		{"new month, same cap", 2, "2026-12-01T00:01:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			now := utc("2026-11-02T10:00:00Z")
			clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
			f := &fakeFetcher{dep: deps(t), arr: arrs(t)}
			b := New(f, 1, 3*time.Hour, clock, quiet())
			limit := 2
			var lmu sync.Mutex
			b.SetCapFunc(func() int { lmu.Lock(); defer lmu.Unlock(); return limit })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tick := make(chan time.Time)
			done := make(chan struct{})
			go func() { b.run(ctx, func() string { return "AGP" }, b.Interval, tick); close(done) }()
			waitCount(t, f, 2)
			mu.Lock()
			now = now.Add(time.Hour)
			mu.Unlock()
			tick <- now // blocked by the cap
			tick <- now // returns only once the blocked tick was handled
			lmu.Lock()
			limit = tc.raise
			lmu.Unlock()
			mu.Lock()
			now = utc(tc.next)
			mu.Unlock()
			tick <- now
			waitCount(t, f, 4)
			cancel()
			<-done
		})
	}
}
