// Package usage meters each outside service's calls, errors and refusals
// per UTC day, so the Sources page can show where every service stands
// against its limit without anyone reading the container logs.
package usage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	recentErrors = 5
	maxMessage   = 120
)

// ErrorEvent is one real failure: a transport error, or a status that means
// the service refused or broke (429, 401/403, 5xx). Never a URL or key.
type ErrorEvent struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
	Subject string    `json:"subject"`
}

// Totals is a meter's state for one UTC day. Recent is newest first.
type Totals struct {
	Day     string       `json:"day"`
	Calls   int          `json:"calls"`
	Errors  int          `json:"errors"`
	Refused int          `json:"refused"`
	Recent  []ErrorEvent `json:"recent_errors"`
}

// Meter counts one service's traffic. It is safe for concurrent use and
// never blocks a request beyond a short mutex hold.
type Meter struct {
	now func() time.Time

	mu      sync.Mutex
	t       Totals
	refused map[string]struct{}
	// changes moves on every update so the registry can skip writing an
	// unchanged file.
	changes uint64
}

func NewMeter(now func() time.Time) *Meter {
	return &Meter{now: now, refused: map[string]struct{}{}}
}

// roll starts a new day's totals at UTC midnight. Caller holds mu.
func (m *Meter) roll() {
	d := m.now().UTC().Format("2006-01-02")
	if d != m.t.Day {
		m.t = Totals{Day: d}
		m.refused = map[string]struct{}{}
	}
}

// Wrap returns a copy of c whose every network round trip is counted. It
// classifies on the response headers alone, so the body is never touched.
func (m *Meter) Wrap(c *http.Client, subject func(*http.Request) string) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp := *c
	cp.Transport = &transport{m: m, base: base, subject: subject}
	return &cp
}

type transport struct {
	m       *Meter
	base    http.RoundTripper
	subject func(*http.Request) string
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A context already done (the tick's time budget spent, or shutdown)
	// fails before any I/O; a cancellation mid-request is our own doing.
	// Neither is a call the service saw or an error it made.
	if req.Context().Err() != nil {
		return t.base.RoundTrip(req)
	}
	resp, err := t.base.RoundTrip(req)
	// http.Client's Timeout cancels the request's context rather than letting
	// it expire, so the error can read "request canceled"; a passed deadline
	// is what tells a timeout from our own cancellation.
	timedOut := false
	if dl, ok := req.Context().Deadline(); ok && err != nil && !time.Now().Before(dl) {
		timedOut = true
	}
	if err != nil && !timedOut && (errors.Is(err, context.Canceled) || req.Context().Err() != nil) {
		return resp, err
	}
	msg := ""
	switch {
	case timedOut:
		msg = "timeout"
	case err != nil:
		msg = errMessage(err)
	case failedStatus(resp.StatusCode):
		msg = fmt.Sprintf("status %d", resp.StatusCode)
	}
	subj := ""
	if t.subject != nil {
		subj = t.subject(req)
	}
	t.m.record(subj, msg)
	return resp, err
}

// failedStatus is true for answers that mean the service refused or broke.
// 400/404 are left out: AeroAPI and adsbdb use them for "no such flight".
func failedStatus(s int) bool {
	return s == http.StatusTooManyRequests || s == http.StatusUnauthorized ||
		s == http.StatusForbidden || s >= 500
}

func errMessage(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	// A *url.Error prints the full URL, query key included; keep the cause.
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		err = ue.Unwrap()
	}
	return trim(err.Error())
}

func trim(s string) string {
	if len(s) > maxMessage {
		return s[:maxMessage]
	}
	return s
}

// record counts one call, and an error when msg is not empty.
func (m *Meter) record(subject, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	m.t.Calls++
	m.changes++
	if msg != "" {
		m.fail(subject, msg)
	}
}

// Fail records an error found after the transport saw a clean answer, such
// as an error envelope inside an HTTP 200 body. It is not a call.
func (m *Meter) Fail(subject, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	m.changes++
	m.fail(subject, trim(msg))
}

func (m *Meter) fail(subject, msg string) {
	m.t.Errors++
	ev := ErrorEvent{At: m.now().UTC(), Message: msg, Subject: subject}
	m.t.Recent = append([]ErrorEvent{ev}, m.t.Recent...)
	if len(m.t.Recent) > recentErrors {
		m.t.Recent = m.t.Recent[:recentErrors]
	}
}

// Refuse records a call a cap or gate stopped. It counts distinct subjects:
// a refused aircraft is asked again every tick, and the useful number is
// how many aircraft missed out, not how many ticks.
func (m *Meter) Refuse(subject string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	if _, seen := m.refused[subject]; seen {
		return
	}
	m.refused[subject] = struct{}{}
	m.t.Refused++
	m.changes++
}

func (m *Meter) Totals() Totals {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	t := m.t
	t.Recent = append([]ErrorEvent{}, m.t.Recent...)
	return t
}

// Restore loads totals saved earlier today; another day's are ignored. The
// refused subjects themselves are not saved, so one refused again after a
// restart is counted again.
func (m *Meter) Restore(t Totals) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	if t.Day != m.t.Day {
		return
	}
	if len(t.Recent) > recentErrors {
		t.Recent = t.Recent[:recentErrors]
	}
	m.t = t
}

func (m *Meter) version() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changes
}
