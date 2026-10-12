package geocode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const malaga = `[{"lat":"36.6785446","lon":"-4.4999552","display_name":"Aeropuerto de Málaga - Costa del Sol, Churriana, Málaga, España","type":"aerodrome"}]`

func server(t *testing.T, calls *int32, body string, status int) (*httptest.Server, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		last = *r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return srv, &last
}

func TestLookup_BuildsTheQueryAndParses(t *testing.T) {
	var calls int32
	srv, last := server(t, &calls, malaga, 200)
	defer srv.Close()
	g := New(srv.Client(), srv.URL, "airdash/test (home display)", time.Now)
	res, ok, err := g.Lookup(context.Background(), "Malaga airport")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if res.Lat < 36.67 || res.Lat > 36.69 || res.Lon > -4.49 || res.Lon < -4.51 || res.Name == "" {
		t.Errorf("result = %+v", res)
	}
	q := last.URL.Query()
	if last.URL.Path != "/search" || q.Get("q") != "Malaga airport" || q.Get("format") != "jsonv2" || q.Get("limit") != "1" || last.Header.Get("User-Agent") != "airdash/test (home display)" {
		t.Errorf("request = %s %v ua=%q", last.URL.Path, q, last.Header.Get("User-Agent"))
	}
}

func TestLookup_NoResultAndErrors(t *testing.T) {
	var calls int32
	srv, _ := server(t, &calls, `[]`, 200)
	defer srv.Close()
	g := New(srv.Client(), srv.URL, "ua", time.Now)
	if _, ok, err := g.Lookup(context.Background(), "nowhere at all"); ok || err != nil {
		t.Errorf("empty result must be a clean miss: ok=%v err=%v", ok, err)
	}
	srv2, _ := server(t, &calls, `oops`, 500)
	defer srv2.Close()
	if _, _, err := New(srv2.Client(), srv2.URL, "ua", time.Now).Lookup(context.Background(), "x y"); err == nil {
		t.Error("500 must error")
	}
	if _, ok, err := g.Lookup(context.Background(), "ab"); ok || err == nil {
		t.Errorf("a query under 3 characters is rejected without a call: ok=%v err=%v", ok, err)
	}
}

func TestLookup_CachesAndThrottles(t *testing.T) {
	var calls int32
	srv, _ := server(t, &calls, malaga, 200)
	defer srv.Close()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	g := New(srv.Client(), srv.URL, "ua", func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if _, ok, err := g.Lookup(context.Background(), "  Malaga Airport "); !ok || err != nil {
			t.Fatalf("lookup %d: ok=%v err=%v", i, ok, err)
		}
	}
	if calls != 1 {
		t.Errorf("same query (trimmed, case-insensitive) must hit the cache: %d calls", calls)
	}
	// A different query within the same second is refused, not queued: the
	// usage policy allows one request per second.
	if _, _, err := g.Lookup(context.Background(), "Torremolinos"); err != ErrThrottled {
		t.Errorf("expected ErrThrottled, got %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, ok, err := g.Lookup(context.Background(), "Torremolinos"); !ok || err != nil || calls != 2 {
		t.Errorf("after a second: ok=%v err=%v calls=%d", ok, err, calls)
	}
}
