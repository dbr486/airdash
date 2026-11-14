package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func newClock(s string) *clock {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &clock{t: t}
}

// statusServer answers each request with the status in its ?s= parameter.
func statusServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, _ := strconv.Atoi(r.URL.Query().Get("s"))
		w.WriteHeader(s)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err == nil {
		resp.Body.Close()
	}
}

func TestMeterCountsCallsAndClassifiesErrors(t *testing.T) {
	clk := newClock("2026-11-02T10:00:00Z")
	m := NewMeter(clk.now)
	srv := statusServer(t)
	c := m.Wrap(&http.Client{}, func(*http.Request) string { return "SUBJ" })
	for _, s := range []int{200, 404, 400, 429, 401, 503} {
		get(t, c, srv.URL+"/?s="+strconv.Itoa(s))
	}
	got := m.Totals()
	if got.Calls != 6 || got.Errors != 3 {
		t.Fatalf("calls=%d errors=%d, want 6 and 3", got.Calls, got.Errors)
	}
	want := []string{"status 503", "status 401", "status 429"}
	if len(got.Recent) != 3 {
		t.Fatalf("recent = %+v", got.Recent)
	}
	for i, w := range want {
		if got.Recent[i].Message != w || got.Recent[i].Subject != "SUBJ" {
			t.Errorf("recent[%d] = %+v, want %q", i, got.Recent[i], w)
		}
	}
}

func TestMeterTimeoutMessage(t *testing.T) {
	m := NewMeter(time.Now)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	get(t, m.Wrap(&http.Client{Timeout: 10 * time.Millisecond}, nil), srv.URL)
	got := m.Totals()
	if got.Calls != 1 || got.Errors != 1 || len(got.Recent) != 1 || got.Recent[0].Message != "timeout" {
		t.Fatalf("totals = %+v", got)
	}
}

func TestMeterKeepsLastFive(t *testing.T) {
	m := NewMeter(time.Now)
	for i := 1; i <= 7; i++ {
		m.Fail("", "e"+strconv.Itoa(i))
	}
	got := m.Totals()
	if len(got.Recent) != 5 || got.Recent[0].Message != "e7" || got.Errors != 7 || got.Calls != 0 {
		t.Fatalf("totals = %+v", got)
	}
}

func TestMeterRefuseDistinct(t *testing.T) {
	m := NewMeter(time.Now)
	m.Refuse("A")
	m.Refuse("A")
	m.Refuse("B")
	if got := m.Totals().Refused; got != 2 {
		t.Fatalf("refused = %d, want 2", got)
	}
}

func TestMeterRollsAtUTCMidnight(t *testing.T) {
	clk := newClock("2026-11-02T23:59:00Z")
	m := NewMeter(clk.now)
	m.Fail("", "x")
	m.Refuse("A")
	clk.set(clk.now().Add(2 * time.Minute))
	got := m.Totals()
	if got.Errors != 0 || got.Refused != 0 || len(got.Recent) != 0 || got.Day != "2026-11-03" {
		t.Fatalf("totals after midnight = %+v", got)
	}
	m.Refuse("A")
	if m.Totals().Refused != 1 {
		t.Fatal("refused set not cleared at rollover")
	}
}

func TestMeterRestoreIgnoresOtherDay(t *testing.T) {
	clk := newClock("2026-11-02T12:00:00Z")
	m := NewMeter(clk.now)
	m.Restore(Totals{Day: "2026-11-01", Calls: 9})
	if m.Totals().Calls != 0 {
		t.Fatal("restored yesterday's totals")
	}
	m.Restore(Totals{Day: "2026-11-02", Calls: 9, Errors: 2, Refused: 3})
	m.Refuse("X")
	got := m.Totals()
	if got.Calls != 9 || got.Errors != 2 || got.Refused != 4 {
		t.Fatalf("totals = %+v", got)
	}
}

// A request whose context is already done (the tick's time budget spent,
// or shutdown) never reaches the network: not a call, not an error.
func TestMeterIgnoresRequestsWithDoneContext(t *testing.T) {
	m := NewMeter(time.Now)
	srv := statusServer(t)
	c := m.Wrap(&http.Client{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/?s=200", nil)
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
	expired, cancel2 := context.WithTimeout(context.Background(), -time.Second)
	defer cancel2()
	req, _ = http.NewRequestWithContext(expired, http.MethodGet, srv.URL+"/?s=200", nil)
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
	if got := m.Totals(); got.Calls != 0 || got.Errors != 0 {
		t.Fatalf("totals = %+v, want nothing recorded", got)
	}
}
