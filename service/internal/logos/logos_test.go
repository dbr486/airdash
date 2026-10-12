package logos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fakePNG = []byte("\x89PNG\r\n\x1a\n fake")

func srv(t *testing.T, calls *int32, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		handler(w, r)
	}))
}

func pngHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(fakePNG)
}

func TestValidCode(t *testing.T) {
	for code, want := range map[string]bool{"BA": true, "U2": true, "ba": false, "BAW": false, "": false, "B/": false} {
		if ValidCode(code) != want {
			t.Errorf("ValidCode(%q) != %v", code, want)
		}
	}
}

func TestGet_HitIsCached(t *testing.T) {
	var calls int32
	var path string
	s := srv(t, &calls, func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path; pngHandler(w, r) })
	defer s.Close()
	src := New(s.Client(), s.URL, "airdash/test", time.Now)
	for i := 0; i < 2; i++ {
		b, ok, err := src.Get(context.Background(), "BA")
		if err != nil || !ok || string(b) != string(fakePNG) {
			t.Fatalf("get %d: ok=%v err=%v body=%q", i, ok, err, b)
		}
	}
	if calls != 1 || path != "/240/80/BA.png" {
		t.Errorf("calls=%d path=%q", calls, path)
	}
}

func TestGet_404IsCachedMiss(t *testing.T) {
	var calls int32
	s := srv(t, &calls, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	defer s.Close()
	src := New(s.Client(), s.URL, "airdash/test", time.Now)
	for i := 0; i < 3; i++ {
		if _, ok, err := src.Get(context.Background(), "ZZ"); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	}
	if calls != 1 {
		t.Errorf("calls=%d", calls)
	}
}

func TestGet_ErrorIsCachedMiss(t *testing.T) {
	var calls int32
	s := srv(t, &calls, pngHandler)
	url := s.URL
	s.Close() // connection refused from now on
	src := New(&http.Client{Timeout: time.Second}, url, "airdash/test", time.Now)
	_, ok, err := src.Get(context.Background(), "BA")
	if ok || err == nil {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	_, ok, err = src.Get(context.Background(), "BA")
	if ok || err != nil {
		t.Errorf("second must be a cached miss without error: ok=%v err=%v", ok, err)
	}
}

func TestGet_NonPNGIsMiss(t *testing.T) {
	var calls int32
	s := srv(t, &calls, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>captive portal</html>"))
	})
	defer s.Close()
	if _, ok, err := New(s.Client(), s.URL, "x", time.Now).Get(context.Background(), "BA"); ok || err != nil {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestGet_OversizeIsMiss(t *testing.T) {
	var calls int32
	s := srv(t, &calls, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(strings.Repeat("x", 64<<10+1)))
	})
	defer s.Close()
	if _, ok, err := New(s.Client(), s.URL, "x", time.Now).Get(context.Background(), "BA"); ok || err != nil {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestGet_InvalidCodeNeverHitsUpstream(t *testing.T) {
	var calls int32
	s := srv(t, &calls, pngHandler)
	defer s.Close()
	src := New(s.Client(), s.URL, "x", time.Now)
	for _, code := range []string{"ba", "BAW", "../x", ""} {
		if _, ok, err := src.Get(context.Background(), code); ok || err != nil {
			t.Errorf("%q: ok=%v err=%v", code, ok, err)
		}
	}
	if calls != 0 {
		t.Errorf("upstream called %d times", calls)
	}
}

func TestGet_EntriesExpire(t *testing.T) {
	var calls int32
	s := srv(t, &calls, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	defer s.Close()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	src := New(s.Client(), s.URL, "x", func() time.Time { return now })
	_, _, _ = src.Get(context.Background(), "ZZ")
	now = now.Add(59 * time.Minute)
	_, _, _ = src.Get(context.Background(), "ZZ")
	if calls != 1 {
		t.Fatalf("miss must be cached for an hour, calls=%d", calls)
	}
	now = now.Add(2 * time.Minute)
	_, _, _ = src.Get(context.Background(), "ZZ")
	if calls != 2 {
		t.Errorf("miss must expire after an hour, calls=%d", calls)
	}
}

func TestGet_CallerCancelDoesNotPoisonCache(t *testing.T) {
	var calls int32
	s := srv(t, &calls, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		pngHandler(w, r)
	})
	defer s.Close()
	src := New(s.Client(), s.URL, "x", time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a browser tab closed or the proxy timed out
	if _, ok, err := src.Get(ctx, "BA"); !ok || err != nil {
		t.Fatalf("cancelled caller must still get the real outcome: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := src.Get(context.Background(), "BA"); !ok || calls != 1 {
		t.Errorf("hit must be cached from the detached fetch: ok=%v calls=%d", ok, calls)
	}
}
