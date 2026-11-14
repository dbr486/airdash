package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportStates(t *testing.T) {
	clk := newClock("2026-11-02T12:00:00Z")
	now := clk.now()
	hit := now.Add(-3 * time.Hour)
	until := now.Add(5 * time.Minute)
	used, capN := 65, 65
	r := NewRegistry(clk.now, nil)
	r.Add("off", "Off", NewMeter(clk.now), Limit{Enabled: false})
	r.Add("paused", "Paused", NewMeter(clk.now), Limit{Enabled: true, PausedUntil: func() time.Time { return until }})
	r.Add("capped", "Capped", NewMeter(clk.now), Limit{Enabled: true, Period: "day",
		Used: func() (int, int, time.Time) { return used, capN, hit }})
	r.Add("ok", "OK", NewMeter(clk.now), Limit{Enabled: true, PausedUntil: func() time.Time { return now.Add(-time.Minute) }})

	got := r.Report()
	if len(got) != 4 {
		t.Fatalf("report = %+v", got)
	}
	if got[0].ID != "off" || got[0].State != "off" {
		t.Errorf("off row = %+v", got[0])
	}
	if got[1].State != "paused" || got[1].StateUntil == nil || !got[1].StateUntil.Equal(until) {
		t.Errorf("paused row = %+v", got[1])
	}
	if got[2].State != "cap_reached" || got[2].StateSince == nil || !got[2].StateSince.Equal(hit) ||
		got[2].Used != 65 || got[2].Cap != 65 || got[2].Period != "day" {
		t.Errorf("capped row = %+v", got[2])
	}
	if got[3].State != "ok" || got[3].StateUntil != nil {
		t.Errorf("ok row = %+v", got[3])
	}

	capN = 100 // the cap raised in Settings
	if s := r.Report()[2]; s.State != "ok" || s.StateSince != nil {
		t.Errorf("after raising cap = %+v", s)
	}
}

func TestReportUncappedUsesCalls(t *testing.T) {
	clk := newClock("2026-11-02T12:00:00Z")
	m := NewMeter(clk.now)
	m.record("", "")
	m.record("", "status 503")
	m.Refuse("A")
	r := NewRegistry(clk.now, nil)
	r.Add("hexdb", "hexdb", m, Limit{Enabled: true, PollS: 5})
	s := r.Report()[0]
	if s.Used != 2 || s.Calls != 2 || s.Cap != 0 || s.Period != "day" || s.Errors != 1 ||
		s.Refused != 1 || len(s.RecentErrors) != 1 || s.PollS != 5 || s.Label != "hexdb" {
		t.Fatalf("row = %+v", s)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	clk := newClock("2026-11-02T12:00:00Z")
	path := filepath.Join(t.TempDir(), "usage.json")
	m := NewMeter(clk.now)
	r := NewRegistry(clk.now, nil)
	r.Add("hexdb", "hexdb", m, Limit{Enabled: true})
	if err := r.Persist(path); err != nil {
		t.Fatal(err)
	}
	m.record("", "")
	m.record("", "timeout")
	r.flush()

	m2 := NewMeter(clk.now)
	r2 := NewRegistry(clk.now, nil)
	r2.Add("hexdb", "hexdb", m2, Limit{Enabled: true})
	if err := r2.Persist(path); err != nil {
		t.Fatal(err)
	}
	got := m2.Totals()
	if got.Calls != 2 || got.Errors != 1 || len(got.Recent) != 1 || got.Recent[0].Message != "timeout" {
		t.Fatalf("restored = %+v", got)
	}
}

func TestPersistMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(time.Now, nil)
	if err := r.Persist(filepath.Join(dir, "none.json")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewRegistry(time.Now, nil).Persist(bad); err == nil {
		t.Fatal("corrupt file accepted")
	}
}

func TestReportRecentErrorsIsEmptyListNotNull(t *testing.T) {
	r := NewRegistry(time.Now, nil)
	r.Add("hexdb", "hexdb", NewMeter(time.Now), Limit{Enabled: true})
	if s := r.Report()[0]; s.RecentErrors == nil {
		t.Fatal("recent_errors must encode as [], not null")
	}
}

func TestReportBlocked(t *testing.T) {
	r := NewRegistry(time.Now, nil)
	r.Add("aeroapi", "FlightAware", NewMeter(time.Now), Limit{Enabled: true,
		Used:    func() (int, int, time.Time) { return 3, 65, time.Time{} },
		Blocked: func() string { return "call count could not be saved" }})
	s := r.Report()[0]
	if s.State != "blocked" || s.StateReason != "call count could not be saved" {
		t.Fatalf("row = %+v", s)
	}
}

// A capped service set to a zero allowance refuses everything: it must not
// read as uncapped and OK.
func TestReportZeroCapIsCappedAndReached(t *testing.T) {
	r := NewRegistry(time.Now, nil)
	r.Add("aeroapi", "FlightAware", NewMeter(time.Now), Limit{Enabled: true,
		Used: func() (int, int, time.Time) { return 0, 0, time.Time{} }})
	r.Add("hexdb", "hexdb", NewMeter(time.Now), Limit{Enabled: true})
	got := r.Report()
	if !got[0].Capped || got[0].State != "cap_reached" {
		t.Errorf("zero cap row = %+v", got[0])
	}
	if got[1].Capped || got[1].State != "ok" {
		t.Errorf("uncapped row = %+v", got[1])
	}
}

func TestReportFreeCap(t *testing.T) {
	clk := newClock("2026-11-02T12:00:00Z")
	limit, used := 0, 5
	r := NewRegistry(clk.now, nil)
	r.Add("hexdb", "hexdb", NewMeter(clk.now), Limit{Enabled: true, ZeroCapIsNone: true,
		Used: func() (int, int, time.Time) { return used, limit, time.Time{} }})
	if s := r.Report()[0]; s.Capped || s.State != "ok" {
		t.Errorf("cap 0 = no cap: %+v", s)
	}
	limit = 5
	if s := r.Report()[0]; !s.Capped || s.State != "cap_reached" || s.Cap != 5 {
		t.Errorf("cap 5 used 5: %+v", s)
	}
}

func TestReportRefreshS(t *testing.T) {
	r := NewRegistry(time.Now, nil)
	r.Add("airlabs", "AirLabs", NewMeter(time.Now), Limit{Enabled: true, Period: "month",
		RefreshEvery: func() time.Duration { return 96 * time.Minute }})
	if s := r.Report()[0]; s.RefreshS != 5760 {
		t.Errorf("refresh_s = %d", s.RefreshS)
	}
}

// AirLabs spends calls in pairs: with one call left (a cap of 1, or 901
// after 900) the board cannot refresh, and the row must say so.
func TestReportExhaustedBelowCap(t *testing.T) {
	r := NewRegistry(time.Now, nil)
	r.Add("airlabs", "AirLabs", NewMeter(time.Now), Limit{Enabled: true, Period: "month",
		Used:      func() (int, int, time.Time) { return 900, 901, time.Time{} },
		Exhausted: func() bool { return true }})
	if s := r.Report()[0]; s.State != "cap_reached" {
		t.Fatalf("row = %+v", s)
	}
}
