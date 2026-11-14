// Package capture records the raw HTTP responses of the route and board
// sources. It sits at the http.RoundTripper so the source clients stay
// untouched and only real network fetches are seen: a cache hit never
// reaches the transport and therefore costs nothing.
package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SourceAirLabs names the AirLabs board source in captured responses.
const SourceAirLabs = "airlabs"

// maxBody bounds what is kept per response: the largest body any source
// client accepts (AirLabs). It is a var so tests can lower it.
var maxBody = 4 << 20

// Response is one upstream answer, as the source sent it.
type Response struct {
	Source, Subject, URL, ContentType string
	Status                            int
	FetchedAt                         time.Time
	Body                              []byte
}

// Sink receives captured responses. Capture runs on the lookup's goroutine,
// so implementations must not block.
type Sink interface{ Capture(Response) }

// Wrap returns a client whose transport reports every completed response to
// sink. A nil sink returns c unchanged, so capture is strictly opt-in.
func Wrap(c *http.Client, sink Sink, source string, subject func(*http.Request) string, now func() time.Time) *http.Client {
	if sink == nil {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp := *c
	cp.Transport = &transport{base: base, sink: sink, source: source, subject: subject, now: now}
	return &cp
}

type transport struct {
	base    http.RoundTripper
	sink    Sink
	source  string
	subject func(*http.Request) string
	now     func() time.Time
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	// Tee rather than drain: the lookups run on the tracker's tick path, so
	// diagnostics must never wait on a body the caller would not have read
	// (e.g. a 404 whose body stalls). What the caller reads is what is kept.
	resp.Body = &teeBody{
		orig: resp.Body,
		record: func(body []byte) {
			if t.source == SourceAirLabs {
				body = StripAirLabsRequest(body)
			}
			t.sink.Capture(Response{
				Source:      t.source,
				Subject:     t.subject(req),
				URL:         RedactURL(req.URL),
				ContentType: resp.Header.Get("Content-Type"),
				Status:      resp.StatusCode,
				FetchedAt:   t.now(),
				Body:        body,
			})
		},
	}
	return resp, nil
}

// teeBody passes the upstream body through and buffers a copy, recording it
// once when the caller reaches EOF or closes the body early. Read and Close
// may run on different goroutines (a caller cancelling a slow read), so the
// copy is guarded by mu; mu is never held across the upstream Read, so Close
// is never stuck behind a stalled body.
type teeBody struct {
	orig   io.ReadCloser
	record func([]byte)

	mu       sync.Mutex
	buf      bytes.Buffer
	oversize bool // over maxBody: keep passing bytes through, never record
	failed   bool // a non-EOF read error: the copy is truncated, never record
	finished bool // recorded or abandoned; later reads must not touch buf
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.orig.Read(p)
	b.mu.Lock()
	if !b.finished {
		if n > 0 && !b.oversize {
			if b.buf.Len()+n > maxBody {
				b.oversize = true
				b.buf = bytes.Buffer{}
			} else {
				b.buf.Write(p[:n])
			}
		}
		if err != nil && err != io.EOF {
			b.failed = true
		}
	}
	b.mu.Unlock()
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

func (b *teeBody) Close() error {
	b.finish()
	return b.orig.Close()
}

func (b *teeBody) finish() {
	b.mu.Lock()
	if b.finished {
		b.mu.Unlock()
		return
	}
	b.finished = true
	var body []byte
	keep := !b.oversize && !b.failed
	if keep {
		body = append([]byte(nil), b.buf.Bytes()...)
	}
	b.buf = bytes.Buffer{}
	b.mu.Unlock()
	if keep {
		b.record(body)
	}
}

// StripAirLabsRequest keeps only the "response" and "error" members of an
// AirLabs v9 envelope. Its "request" member echoes the API key (and the
// caller's IP and location), which must never reach the browsable log.
// It fails closed: a body that does not decode as a JSON object (truncated,
// BOM-prefixed, an array, plain text) cannot be filtered, so it is dropped.
func StripAirLabsRequest(body []byte) []byte {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil || env == nil {
		return nil
	}
	for k := range env {
		if k != "response" && k != "error" {
			delete(env, k)
		}
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return out
}

// secretParams are query keys that carry credentials; stored URLs must never
// contain them, since the log is browsable from the dashboard.
var secretParams = map[string]bool{"api_key": true, "apikey": true, "key": true, "token": true}

// RedactURL renders u without credential query parameters. The remaining
// query keeps its original order and encoding.
func RedactURL(u *url.URL) string {
	cp := *u
	cp.User = nil
	var kept []string
	for _, pair := range strings.Split(u.RawQuery, "&") {
		if pair == "" {
			continue
		}
		name, _, _ := strings.Cut(pair, "=")
		if n, err := url.QueryUnescape(name); err == nil {
			name = n
		}
		if secretParams[strings.ToLower(name)] {
			continue
		}
		kept = append(kept, pair)
	}
	cp.RawQuery = strings.Join(kept, "&")
	return cp.String()
}

// SubjectLastSegment keys adsbdb and aeroapi requests by the final path
// element, e.g. /v0/callsign/RYR5EL -> RYR5EL.
func SubjectLastSegment(r *http.Request) string {
	p := strings.TrimRight(r.URL.Path, "/")
	return p[strings.LastIndex(p, "/")+1:]
}

// SubjectHexDB keys route lookups by callsign and prefixes airport lookups so
// the two kinds cannot collide in the same source.
func SubjectHexDB(r *http.Request) string {
	s := SubjectLastSegment(r)
	if strings.Contains(r.URL.Path, "/airport/") {
		return "airport:" + s
	}
	return s
}

// SubjectAirLabs names a board request by the airport and direction queried.
func SubjectAirLabs(r *http.Request) string {
	q := r.URL.Query()
	for _, k := range []string{"dep_iata", "dep_icao"} {
		if v := q.Get(k); v != "" {
			return "dep:" + v
		}
	}
	for _, k := range []string{"arr_iata", "arr_icao"} {
		if v := q.Get(k); v != "" {
			return "arr:" + v
		}
	}
	return ""
}
