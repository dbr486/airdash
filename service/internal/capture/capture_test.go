package capture

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type recSink struct {
	mu  sync.Mutex
	got []Response
}

func (s *recSink) Capture(r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
}

func (s *recSink) all() []Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Response(nil), s.got...)
}

var fakeNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return fakeNow }

func TestWrapRecordsAndPassesBodyThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, "adsbdb", SubjectLastSegment, clock)

	resp, err := c.Get(srv.URL + "/v0/callsign/RYR5EL")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"a":1}` {
		t.Fatalf("caller body = %q", body)
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("captured %d, want 1", len(got))
	}
	r := got[0]
	if r.Source != "adsbdb" || r.Subject != "RYR5EL" || r.Status != 200 ||
		r.ContentType != "application/json" || string(r.Body) != `{"a":1}` || !r.FetchedAt.Equal(fakeNow) {
		t.Fatalf("unexpected capture: %+v", r)
	}
	if r.URL != srv.URL+"/v0/callsign/RYR5EL" {
		t.Fatalf("URL = %q", r.URL)
	}
}

func TestWrapRecordsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, "adsbdb", SubjectLastSegment, clock)
	resp, err := c.Get(srv.URL + "/v0/callsign/X")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	got := sink.all()
	if len(got) != 1 || got[0].Status != 404 || string(got[0].Body) != "nope" {
		t.Fatalf("got %+v", got)
	}
}

func TestWrapTransportErrorRecordsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	client := srv.Client()
	srv.Close()
	sink := &recSink{}
	c := Wrap(client, sink, "adsbdb", SubjectLastSegment, clock)
	if resp, err := c.Get(url + "/v0/callsign/X"); err == nil {
		resp.Body.Close()
		t.Fatal("expected error")
	}
	if n := len(sink.all()); n != 0 {
		t.Fatalf("captured %d, want 0", n)
	}
}

func TestWrapOversizeBodyNotRecorded(t *testing.T) {
	old := maxBody
	maxBody = 100
	defer func() { maxBody = old }()
	payload := strings.Repeat("x", maxBody+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, "adsbdb", SubjectLastSegment, clock)
	resp, err := c.Get(srv.URL + "/v0/callsign/X")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != payload {
		t.Fatalf("caller got %d bytes, err %v", len(body), err)
	}
	if n := len(sink.all()); n != 0 {
		t.Fatalf("captured %d, want 0", n)
	}
}

func TestWrapNilSinkReturnsSameClient(t *testing.T) {
	c := &http.Client{}
	if Wrap(c, nil, "x", SubjectLastSegment, clock) != c {
		t.Fatal("want same client")
	}
}

func TestRedactURL(t *testing.T) {
	u, _ := url.Parse("https://airlabs.co/api/v9/schedules?dep_iata=AGP&API_KEY=s&limit=500&token=t")
	got := RedactURL(u)
	want := "https://airlabs.co/api/v9/schedules?dep_iata=AGP&limit=500"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if strings.Contains(got, "=s") || strings.Contains(got, "=t") {
		t.Fatalf("leaked secret: %q", got)
	}
}

func TestSubjects(t *testing.T) {
	req := func(s string) *http.Request {
		r, err := http.NewRequest("GET", s, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cases := []struct {
		name string
		fn   func(*http.Request) string
		url  string
		want string
	}{
		{"last", SubjectLastSegment, "https://x/v0/callsign/RYR5EL", "RYR5EL"},
		{"last escaped", SubjectLastSegment, "https://x/v0/callsign/AB%20C", "AB C"},
		{"last trailing slash", SubjectLastSegment, "https://x/v0/callsign/RYR5EL/", "RYR5EL"},
		{"hexdb route", SubjectHexDB, "https://x/api/v1/route/icao/RYR5EL", "RYR5EL"},
		{"hexdb airport", SubjectHexDB, "https://x/api/v1/airport/icao/LEMG", "airport:LEMG"},
		{"hexdb trailing slash", SubjectHexDB, "https://x/api/v1/airport/icao/LEMG/", "airport:LEMG"},
		{"hexdb escaped", SubjectHexDB, "https://x/api/v1/route/icao/AB%20C", "AB C"},
		{"airlabs dep", SubjectAirLabs, "https://x/schedules?dep_iata=AGP&limit=5", "dep:AGP"},
		{"airlabs arr icao", SubjectAirLabs, "https://x/schedules?arr_icao=LEMG", "arr:LEMG"},
		{"airlabs none", SubjectAirLabs, "https://x/schedules?limit=5", ""},
	}
	for _, c := range cases {
		if got := c.fn(req(c.url)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestWrapDoesNotWaitForUnreadBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	sink := &recSink{}
	base := srv.Client()
	base.Timeout = 3 * time.Second
	c := Wrap(base, sink, "adsbdb", SubjectLastSegment, clock)

	start := time.Now()
	resp, err := c.Get(srv.URL + "/v0/callsign/X")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("lookup took %v; capture must not drain the body", d)
	}
	got := sink.all()
	if len(got) != 1 || got[0].Status != 404 || len(got[0].Body) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestWrapRecordsOnceOnRepeatedClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, "adsbdb", SubjectLastSegment, clock)
	resp, err := c.Get(srv.URL + "/v0/callsign/X")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp.Body.Close()
	got := sink.all()
	if len(got) != 1 || string(got[0].Body) != "hello" {
		t.Fatalf("got %+v", got)
	}
}

func TestWrapTruncatedBodyNotRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, "adsbdb", SubjectLastSegment, clock)
	resp, err := c.Get(srv.URL + "/v0/callsign/X")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("expected read error")
	}
	resp.Body.Close()
	if n := len(sink.all()); n != 0 {
		t.Fatalf("captured %d, want 0", n)
	}
}

func TestWrapStripsAirLabsRequestEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request":{"key":{"api_key":"SECRET","id":1},"client":{"ip":"1.2.3.4"}},"response":[{"flight_iata":"FR1"}],"terms":"x"}`))
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, SourceAirLabs, SubjectAirLabs, clock)
	resp, err := c.Get(srv.URL + "/schedules?dep_iata=AGP")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "SECRET") {
		t.Fatal("caller must still get the untouched body")
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("captured %d, want 1", len(got))
	}
	stored := string(got[0].Body)
	if strings.Contains(stored, "SECRET") || strings.Contains(stored, "1.2.3.4") || strings.Contains(stored, "terms") {
		t.Fatalf("stored body leaks the request echo: %s", stored)
	}
	if !strings.Contains(stored, `"flight_iata":"FR1"`) {
		t.Fatalf("stored body lost the response rows: %s", stored)
	}
}

func TestStripAirLabsRequest(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"error":{"code":"x"},"request":{"key":"k"}}`, `{"error":{"code":"x"}}`},
		// Anything that is not a JSON object cannot be filtered, so nothing
		// of it is kept.
		{`[1,2]`, ``},
		{`null`, ``},
		{`"text"`, ``},
		{`not json`, ``},
		{``, ``},
		{"\ufeff" + `{"request":{"key":{"api_key":"SECRET"}},"response":[]}`, ``},
		{`[{"request":{"key":{"api_key":"SECRET"}},"response":[]}]`, ``},
		{`upstream said api_key=SECRET`, ``},
		{`{"request":{"key":{"api_key":"SECRET"`, ``},
	}
	for _, c := range cases {
		got := string(StripAirLabsRequest([]byte(c.in)))
		if got != c.want || strings.Contains(got, "SECRET") {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestWrapConcurrentReadAndClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := w.Write([]byte("chunk")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}))
	defer srv.Close()
	sink := &recSink{}
	c := Wrap(srv.Client(), sink, "adsbdb", SubjectLastSegment, clock)
	resp, err := c.Get(srv.URL + "/v0/callsign/X")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 3)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		resp.Body.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop after Close")
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("captured %d, want 1", len(got))
	}
	snapshot := string(got[0].Body)
	resp.Body.Close()
	_, _ = resp.Body.Read(make([]byte, 8))
	if got := sink.all(); len(got) != 1 || string(got[0].Body) != snapshot {
		t.Fatalf("recorded capture changed after Close: %d captures", len(got))
	}
	if !strings.HasPrefix(snapshot, "chu") {
		t.Fatalf("recorded %q", snapshot)
	}
}
