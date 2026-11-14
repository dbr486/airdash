package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const flushInterval = 30 * time.Second

// Limit tells the registry how a service is capped. The cap itself stays
// with its owner (budget, board, gate); these funcs only read it, so a cap
// changed in Settings shows at once. Nil funcs mean "not applicable".
type Limit struct {
	Enabled bool
	// Period is "day" or "month": what Used counts against. Empty is "day".
	Period string
	// Used returns the count the cap is measured against, the cap (0 = none)
	// and when the cap was first hit this period (zero if not).
	Used        func() (used, cap int, capHitAt time.Time)
	PausedUntil func() time.Time
	// Blocked returns why calls are refused below the cap ("" if not), e.g.
	// a paid counter that fails closed because it cannot be saved.
	Blocked func() string
	PollS   int
}

// Service is one row of GET /api/limits.
type Service struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Period  string `json:"period"`
	Used    int    `json:"used"`
	Cap     int    `json:"cap"`
	// Capped tells a service with a cap (even a zero allowance) from one
	// with none, where cap is 0 too.
	Capped       bool         `json:"capped"`
	State        string       `json:"state"`
	StateSince   *time.Time   `json:"state_since"`
	StateUntil   *time.Time   `json:"state_until"`
	StateReason  string       `json:"state_reason,omitempty"`
	Refused      int          `json:"refused"`
	Calls        int          `json:"calls"`
	Errors       int          `json:"errors"`
	RecentErrors []ErrorEvent `json:"recent_errors"`
	PollS        int          `json:"poll_s,omitempty"`
}

type entry struct {
	id, label string
	m         *Meter
	l         Limit
}

// Registry lists every metered service in display order and keeps today's
// totals on disk, so a redeploy does not blank the page.
type Registry struct {
	now    func() time.Time
	logger *slog.Logger

	mu      sync.Mutex
	entries []entry
	path    string
	written map[string]uint64 // meter versions in the last file written
}

func NewRegistry(now func() time.Time, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{now: now, logger: logger, written: map[string]uint64{}}
}

func (r *Registry) Add(id, label string, m *Meter, l Limit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{id: id, label: label, m: m, l: l})
}

func (r *Registry) list() []entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]entry(nil), r.entries...)
}

// Report works each state out live: off, blocked, paused, cap reached.
func (r *Registry) Report() []Service {
	now := r.now()
	out := []Service{}
	for _, e := range r.list() {
		t := e.m.Totals()
		s := Service{ID: e.id, Label: e.label, Enabled: e.l.Enabled, Period: e.l.Period,
			Used: t.Calls, Refused: t.Refused, Calls: t.Calls, Errors: t.Errors,
			RecentErrors: t.Recent, PollS: e.l.PollS, State: "ok"}
		if s.Period == "" {
			s.Period = "day"
		}
		var hitAt time.Time
		if e.l.Used != nil {
			s.Used, s.Cap, hitAt = e.l.Used()
			s.Capped = true
		}
		var until time.Time
		if e.l.PausedUntil != nil {
			until = e.l.PausedUntil()
		}
		blocked := ""
		if e.l.Enabled && e.l.Blocked != nil {
			blocked = e.l.Blocked()
		}
		switch {
		case !e.l.Enabled:
			s.State = "off"
		case blocked != "":
			s.State, s.StateReason = "blocked", blocked
		case until.After(now):
			s.State, s.StateUntil = "paused", &until
		case s.Capped && s.Used >= s.Cap:
			s.State = "cap_reached"
			if !hitAt.IsZero() {
				s.StateSince = &hitAt
			}
		}
		out = append(out, s)
	}
	return out
}

// Persist restores today's totals from path, if present, and makes Run and
// flush write there. A corrupt file is an error the caller logs; metering
// then starts from zero.
func (r *Registry) Persist(path string) error {
	r.mu.Lock()
	r.path = path
	r.mu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved map[string]Totals
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("usage state: %w", err)
	}
	for _, e := range r.list() {
		if t, ok := saved[e.id]; ok {
			e.m.Restore(t)
		}
	}
	return nil
}

// Run writes changed totals every 30 s and once more when ctx ends. Up to
// 30 s of diagnostics lost on a crash is the price of keeping writes off
// the request path.
func (r *Registry) Run(ctx context.Context) {
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			r.flush()
		case <-ctx.Done():
			r.flush()
			return
		}
	}
}

func (r *Registry) flush() {
	r.mu.Lock()
	path := r.path
	r.mu.Unlock()
	if path == "" {
		return
	}
	state := map[string]Totals{}
	versions := map[string]uint64{}
	changed := false
	for _, e := range r.list() {
		v := e.m.version()
		versions[e.id] = v
		state[e.id] = e.m.Totals()
		r.mu.Lock()
		if r.written[e.id] != v {
			changed = true
		}
		r.mu.Unlock()
	}
	if !changed {
		return
	}
	data, err := json.Marshal(state)
	if err == nil {
		tmp := filepath.Join(filepath.Dir(path), ".usage.tmp")
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		r.logger.Warn("usage state not saved", "err", err)
		return
	}
	r.mu.Lock()
	r.written = versions
	r.mu.Unlock()
}
