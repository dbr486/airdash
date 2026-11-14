package routes

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"header seconds", "120", 120 * time.Second},
		{"absent", "", defaultRateLimit},
		{"http date", "Sat, 31 Oct 2026 19:00:00 GMT", defaultRateLimit},
		{"absurd", "99999999999999", maxRateLimitWait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Retry-After", tc.header)
			}
			if got := retryAfter(h); got != tc.want {
				t.Errorf("retryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

// gateServer answers with the status in *status and counts every hit.
func gateServer(t *testing.T, status *atomic.Int32, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func gateGet(t *testing.T, c *http.Client, url string) (int, string, error) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestGate_ClosesOn429AndReopensAfterWindow(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	body := `{"response":"rate limited for 300 seconds"}`
	srv, hits := gateServer(t, &status, body)
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	g := NewGate(ck.now, slog.New(slog.DiscardHandler))
	c := g.Wrap(srv.Client())

	code, got, err := gateGet(t, c, srv.URL)
	if err != nil || code != http.StatusTooManyRequests || got != body {
		t.Fatalf("first call: code=%d body=%q err=%v; the 429 body must reach the caller", code, got, err)
	}

	// Closed: no request reaches the server, and the error reads as a cooldown.
	ck.advance(300 * time.Second)
	_, _, err = gateGet(t, c, srv.URL)
	if !errors.Is(err, ErrRateLimited) || !errors.Is(err, ErrCooldown) {
		t.Fatalf("while closed err = %v, want ErrRateLimited wrapping ErrCooldown", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hit %d times while closed, want 1", hits.Load())
	}

	status.Store(http.StatusOK)
	ck.advance(15*time.Second + time.Millisecond)
	if code, _, err := gateGet(t, c, srv.URL); err != nil || code != http.StatusOK {
		t.Fatalf("after window: code=%d err=%v", code, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("server hit %d times, want 2", hits.Load())
	}
}

func TestGate_RepeatDoublesAndSuccessResets(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	srv, _ := gateServer(t, &status, `{"response":"rate limited for 300 seconds"}`)
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	g := NewGate(ck.now, slog.New(slog.DiscardHandler))
	c := g.Wrap(srv.Client())

	open := func() bool {
		_, _, err := gateGet(t, c, srv.URL)
		return !errors.Is(err, ErrRateLimited)
	}
	first := 315*time.Second + time.Millisecond

	open() // 429 #1: closed for 300s+15s
	ck.advance(first)
	if !open() { // 429 #2: closed for 600s+15s
		t.Fatal("gate still closed after the first window")
	}
	ck.advance(first)
	if open() {
		t.Fatal("second window should be doubled")
	}
	ck.advance(300 * time.Second)
	status.Store(http.StatusOK)
	if !open() { // 200 resets the streak
		t.Fatal("gate still closed after the doubled window")
	}
	status.Store(http.StatusTooManyRequests)
	open() // 429 after reset: back to 300s+15s
	ck.advance(first)
	if !open() {
		t.Fatal("streak was not reset by the 200")
	}
}

func TestGate_WindowIsCapped(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	srv, _ := gateServer(t, &status, `{"response":"rate limited for 300 seconds"}`)
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	g := NewGate(ck.now, slog.New(slog.DiscardHandler))
	c := g.Wrap(srv.Client())

	for i := 0; i < 10; i++ {
		_, _, _ = gateGet(t, c, srv.URL)
		ck.advance(maxRateLimitWait + time.Millisecond)
	}
	status.Store(http.StatusOK)
	if code, _, err := gateGet(t, c, srv.URL); err != nil || code != http.StatusOK {
		t.Fatalf("after a capped window: code=%d err=%v", code, err)
	}
}

func TestGate_SharedAcrossClients(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	srv, hits := gateServer(t, &status, `{"response":"rate limited for 300 seconds"}`)
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	g := NewGate(ck.now, slog.New(slog.DiscardHandler))
	routeClient, ownerClient := g.Wrap(srv.Client()), g.Wrap(srv.Client())

	_, _, _ = gateGet(t, routeClient, srv.URL)
	if _, _, err := gateGet(t, ownerClient, srv.URL); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("owner client err = %v, want ErrRateLimited after a route 429", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1", hits.Load())
	}
}

func TestGate_TransportErrorLeavesGateOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	g := NewGate(ck.now, slog.New(slog.DiscardHandler))
	c := g.Wrap(&http.Client{})

	for i := 0; i < 2; i++ {
		if _, _, err := gateGet(t, c, url); err == nil || errors.Is(err, ErrRateLimited) {
			t.Fatalf("call %d: err = %v, want a plain transport error", i, err)
		}
	}
}

// After a ban only one probe may be in flight: a second caller arriving
// while the probe waits for its answer must be refused, not sent.
func TestGate_OneProbeAfterBan(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 2 {
			<-release // hold the probe in flight
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return ck.now() }
	g := NewGate(now, slog.New(slog.DiscardHandler))
	c := g.Wrap(srv.Client())

	_, _, _ = gateGet(t, c, srv.URL) // 429: closes
	mu.Lock()
	ck.advance(316 * time.Second)
	mu.Unlock()
	status.Store(http.StatusOK)

	probeDone := make(chan error, 1)
	go func() { _, _, err := gateGet(t, c, srv.URL); probeDone <- err }()
	for hits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if _, _, err := gateGet(t, c, srv.URL); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second caller during probe: err = %v, want ErrRateLimited", err)
	}
	close(release)
	if err := <-probeDone; err != nil {
		t.Fatalf("probe: %v", err)
	}
	if code, _, err := gateGet(t, c, srv.URL); err != nil || code != http.StatusOK {
		t.Fatalf("after a good probe: code=%d err=%v", code, err)
	}
	if hits.Load() != 3 {
		t.Fatalf("server hit %d times, want 3", hits.Load())
	}
}

// A success admitted before a ban but answered after it says nothing about
// the ban, so it must not clear the streak and with it the single probe.
func TestGate_StaleSuccessKeepsStreak(t *testing.T) {
	ck := &clock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	g := NewGate(ck.now, slog.New(slog.DiscardHandler))
	okA, probeA := g.admit() // admitted before the ban
	okB, probeB := g.admit()
	if !okA || !okB || probeA || probeB {
		t.Fatalf("open gate: admit = (%v,%v) (%v,%v)", okA, probeA, okB, probeB)
	}
	g.done(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, nil, probeB)
	g.done(&http.Response{StatusCode: http.StatusOK}, nil, probeA)

	ck.advance(316 * time.Second)
	if ok, probe := g.admit(); !ok || !probe {
		t.Fatalf("first caller after the ban: ok=%v probe=%v, want the probe", ok, probe)
	}
	if ok, _ := g.admit(); ok {
		t.Fatal("second caller admitted while the probe is out")
	}
}
