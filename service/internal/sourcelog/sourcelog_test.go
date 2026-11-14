package sourcelog

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airdash/internal/tracker"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func openTest(t *testing.T) (*Log, *clock) {
	t.Helper()
	c := &clock{t: t0}
	l, err := Open(filepath.Join(t.TempDir(), "sources.db"), c.now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, c
}

func ans(status, route string) tracker.SourceAnswer {
	return tracker.SourceAnswer{Status: status, Route: route}
}

func rec(hex string, first time.Time) tracker.PassRecord {
	return tracker.PassRecord{
		Hex: hex, Callsign: "BAW1", Reg: "G-AAAA", Type: "A320", Airline: "BA",
		FirstSeen: first, LastSeen: first, DistKm: 10, AltFt: 5000,
		Board:       ans(tracker.StatusMiss, ""),
		ADSBDB:      ans(tracker.StatusUnknown, ""),
		HexDB:       ans(tracker.StatusUnknown, ""),
		AeroAPI:     ans(tracker.StatusNotAsked, ""),
		ShownSource: tracker.SourceNone,
	}
}

func mustWrite(t *testing.T, l *Log, recs ...tracker.PassRecord) {
	t.Helper()
	if err := l.write(recs); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func onlyPass(t *testing.T, l *Log) Pass {
	t.Helper()
	ps, err := l.Passes(context.Background(), 10, time.Time{}, "")
	if err != nil {
		t.Fatalf("Passes: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("got %d passes, want 1", len(ps))
	}
	return ps[0]
}

func TestUpsertMergeRules(t *testing.T) {
	l, _ := openTest(t)

	a := rec("abc123", t0.Add(-time.Hour))
	a.ADSBDB = ans(tracker.StatusHit, "LHR-JFK")
	a.Board = ans(tracker.StatusMiss, "")
	a.ShownSource, a.ShownRoute = tracker.SourceADSBDB, "LHR-JFK"
	a.Nearest = true
	a.DistKm, a.AltFt = 8, 4000
	mustWrite(t, l, a)

	b := a
	b.LastSeen = a.FirstSeen.Add(5 * time.Minute)
	b.ADSBDB = ans(tracker.StatusError, "")         // a hit is kept
	b.Board = ans(tracker.StatusHit, "LHR-BOS")     // latest wins
	b.HexDB = ans(tracker.StatusImplausible, "X-Y") // latest wins
	b.ShownSource, b.ShownRoute = tracker.SourceNone, ""
	b.Nearest = false
	b.DistKm, b.AltFt = 12, 6000
	b.Callsign, b.Reg, b.Type, b.Airline = "", "", "", ""
	mustWrite(t, l, b)

	p := onlyPass(t, l)
	if p.ADSBDB != (Answer{tracker.StatusHit, "LHR-JFK"}) {
		t.Errorf("adsbdb hit replaced: %+v", p.ADSBDB)
	}
	if p.Board != (Answer{tracker.StatusHit, "LHR-BOS"}) {
		t.Errorf("board = %+v", p.Board)
	}
	if p.HexDB != (Answer{tracker.StatusImplausible, "X-Y"}) {
		t.Errorf("hexdb = %+v", p.HexDB)
	}
	if p.ShownSource != tracker.SourceADSBDB || p.ShownRoute != "LHR-JFK" {
		t.Errorf("shown replaced by none: %s %s", p.ShownSource, p.ShownRoute)
	}
	if !p.Nearest {
		t.Error("nearest not sticky")
	}
	if p.MinDistKm != 8 || p.AltFt != 4000 {
		t.Errorf("min dist/alt = %v/%v", p.MinDistKm, p.AltFt)
	}
	if p.LastSeen != b.LastSeen.Unix() || p.FirstSeen != a.FirstSeen.Unix() {
		t.Errorf("seen = %d..%d", p.FirstSeen, p.LastSeen)
	}
	if p.Callsign != "BAW1" || p.Reg != "G-AAAA" || p.Type != "A320" || p.Airline != "BA" {
		t.Errorf("empty context overwrote: %+v", p)
	}

	// A later, non-none shown answer and non-empty context replace.
	c := b
	c.LastSeen = a.FirstSeen.Add(2 * time.Minute) // older than stored
	c.ShownSource, c.ShownRoute = tracker.SourceBoard, "LHR-BOS"
	c.Callsign, c.DistKm, c.AltFt = "BAW2", 3, 2000
	c.Board = ans(tracker.StatusMiss, "") // a hit is kept
	mustWrite(t, l, c)

	p = onlyPass(t, l)
	if p.ShownSource != tracker.SourceBoard || p.ShownRoute != "LHR-BOS" {
		t.Errorf("shown = %s %s", p.ShownSource, p.ShownRoute)
	}
	if p.Callsign != "BAW2" {
		t.Errorf("callsign = %q", p.Callsign)
	}
	if p.LastSeen != b.LastSeen.Unix() {
		t.Errorf("last_seen went back: %d", p.LastSeen)
	}
	if p.MinDistKm != 3 || p.AltFt != 2000 {
		t.Errorf("min dist/alt = %v/%v", p.MinDistKm, p.AltFt)
	}
	if p.Board != (Answer{tracker.StatusHit, "LHR-BOS"}) {
		t.Errorf("board hit replaced: %+v", p.Board)
	}

	// A different first_seen is a new pass.
	mustWrite(t, l, rec("abc123", t0))
	ps, _ := l.Passes(context.Background(), 10, time.Time{}, "")
	if len(ps) != 2 {
		t.Fatalf("passes = %d, want 2", len(ps))
	}
}

func TestPrune(t *testing.T) {
	l, c := openTest(t)
	mustWrite(t, l,
		rec("old", t0.Add(-91*24*time.Hour)),
		rec("new", t0.Add(-89*24*time.Hour)),
	)
	if err := l.prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	p := onlyPass(t, l)
	if p.Hex != "new" {
		t.Errorf("kept %q", p.Hex)
	}

	c.set(t0.Add(2 * 24 * time.Hour))
	if err := l.prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	ps, _ := l.Passes(context.Background(), 10, time.Time{}, "")
	if len(ps) != 0 {
		t.Errorf("passes = %d, want 0", len(ps))
	}
}

func TestSummary(t *testing.T) {
	l, _ := openTest(t)
	yesterday := time.Date(2026, 10, 29, 23, 59, 0, 0, time.UTC)

	// Seen across midnight: counted on the day of first_seen only.
	p1 := rec("a", yesterday)
	p1.LastSeen = yesterday.Add(10 * time.Minute)
	p1.Board = ans(tracker.StatusHit, "LHR-JFK")
	p1.ADSBDB = ans(tracker.StatusImplausible, "LHR-JFK") // answered, agrees
	p1.HexDB = ans(tracker.StatusHit, "LHR-BOS")          // disagrees
	p1.ShownSource, p1.ShownRoute = tracker.SourceBoard, "LHR-JFK"

	// AeroAPI is the reference when the board misses.
	p2 := rec("b", t0.Add(-2*time.Hour))
	p2.AeroAPI = ans(tracker.StatusHit, "AMS-CDG")
	p2.ADSBDB = ans(tracker.StatusHit, "AMS-NCE")
	p2.HexDB = ans(tracker.StatusUnknown, "")
	p2.ShownSource, p2.ShownRoute = tracker.SourceAeroAPI, "AMS-CDG"

	// No reference: no agreement counted.
	p3 := rec("c", t0.Add(-time.Hour))
	p3.ADSBDB = ans(tracker.StatusHit, "AMS-NCE")

	// Outside the range.
	p4 := rec("d", t0.Add(-10*24*time.Hour))

	mustWrite(t, l, p1, p2, p3, p4)

	s, err := l.Summary(context.Background(), 7)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(s.Days) != 2 {
		t.Fatalf("days = %+v", s.Days)
	}
	d0, d1 := s.Days[0], s.Days[1]
	if d0.Day != "2026-10-29" || d0.Passes != 1 || d1.Day != "2026-10-30" || d1.Passes != 2 {
		t.Errorf("days = %s/%d %s/%d", d0.Day, d0.Passes, d1.Day, d1.Passes)
	}
	if d0.Agreement["adsbdb"] != (Agreement{Agree: 1}) || d0.Agreement["hexdb"] != (Agreement{Disagree: 1}) {
		t.Errorf("day0 agreement = %+v", d0.Agreement)
	}
	if d1.Agreement["adsbdb"] != (Agreement{Disagree: 1}) || d1.Agreement["hexdb"] != (Agreement{}) {
		t.Errorf("day1 agreement = %+v", d1.Agreement)
	}
	if d1.Sources["adsbdb"]["hit"] != 2 || d1.Sources["board"]["miss"] != 2 || d1.Sources["aeroapi"]["not_asked"] != 1 {
		t.Errorf("day1 sources = %+v", d1.Sources)
	}
	if d1.Shown["aeroapi"] != 1 || d1.Shown["none"] != 1 {
		t.Errorf("day1 shown = %+v", d1.Shown)
	}

	tot := s.Totals
	if tot.Day != "" || tot.Passes != 3 {
		t.Errorf("totals = %q/%d", tot.Day, tot.Passes)
	}
	if tot.Agreement["adsbdb"] != (Agreement{Agree: 1, Disagree: 1}) {
		t.Errorf("totals agreement = %+v", tot.Agreement)
	}
	if tot.Sources["hexdb"]["hit"] != 1 || tot.Sources["hexdb"]["unknown"] != 2 || tot.Shown["board"] != 1 {
		t.Errorf("totals = %+v", tot)
	}

	// days=1 is today only.
	s, _ = l.Summary(context.Background(), 1)
	if len(s.Days) != 1 || s.Totals.Passes != 2 {
		t.Errorf("days=1: %+v", s)
	}
}

func TestSummaryEmpty(t *testing.T) {
	l, _ := openTest(t)
	s, err := l.Summary(context.Background(), 7)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(s.Days) != 0 || s.Totals.Passes != 0 {
		t.Errorf("summary = %+v", s)
	}
	if s.Days == nil {
		t.Error("Days is nil; want an empty list in JSON")
	}
}

func TestDisagreements(t *testing.T) {
	l, _ := openTest(t)

	agree := rec("agree", t0.Add(-3*time.Hour))
	agree.Board = ans(tracker.StatusHit, "LHR-JFK")
	agree.ADSBDB = ans(tracker.StatusHit, "LHR-JFK")

	differ := rec("differ", t0.Add(-2*time.Hour))
	differ.ADSBDB = ans(tracker.StatusImplausible, "LHR-JFK")
	differ.HexDB = ans(tracker.StatusHit, "LHR-BOS")

	single := rec("single", t0.Add(-time.Hour))
	single.ADSBDB = ans(tracker.StatusHit, "LHR-JFK")
	single.HexDB = ans(tracker.StatusUnknown, "")

	differ2 := rec("differ2", t0.Add(-30*time.Minute))
	differ2.Board = ans(tracker.StatusHit, "AMS-CDG")
	differ2.AeroAPI = ans(tracker.StatusHit, "AMS-NCE")

	old := rec("old", t0.Add(-5*24*time.Hour))
	old.ADSBDB = ans(tracker.StatusHit, "A-B")
	old.HexDB = ans(tracker.StatusHit, "A-C")

	mustWrite(t, l, agree, differ, single, differ2, old)

	ps, err := l.Disagreements(context.Background(), 2, 10)
	if err != nil {
		t.Fatalf("Disagreements: %v", err)
	}
	if len(ps) != 2 || ps[0].Hex != "differ2" || ps[1].Hex != "differ" {
		t.Errorf("got %v", hexes(ps))
	}

	ps, _ = l.Disagreements(context.Background(), 2, 1)
	if len(ps) != 1 || ps[0].Hex != "differ2" {
		t.Errorf("limit 1: %v", hexes(ps))
	}
	ps, _ = l.Disagreements(context.Background(), 7, 10)
	if len(ps) != 3 {
		t.Errorf("days 7: %v", hexes(ps))
	}
}

func hexes(ps []Pass) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Hex)
	}
	return out
}

func TestPassesOrderBeforeLimit(t *testing.T) {
	l, _ := openTest(t)
	for i, h := range []string{"a", "b", "c", "d"} {
		mustWrite(t, l, rec(h, t0.Add(time.Duration(i-4)*time.Minute)))
	}
	ps, err := l.Passes(context.Background(), 2, time.Time{}, "")
	if err != nil {
		t.Fatalf("Passes: %v", err)
	}
	if got := hexes(ps); len(got) != 2 || got[0] != "d" || got[1] != "c" {
		t.Errorf("newest: %v", got)
	}
	ps, _ = l.Passes(context.Background(), 10, t0.Add(-2*time.Minute), "")
	if got := hexes(ps); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("before: %v", got)
	}
}

func TestClamp(t *testing.T) {
	l, _ := openTest(t)
	for i := 0; i < 3; i++ {
		mustWrite(t, l, rec("h", t0.Add(-time.Duration(i)*time.Minute)))
	}
	mustWrite(t, l, rec("old", t0.Add(-89*24*time.Hour)))

	ps, err := l.Passes(context.Background(), 0, time.Time{}, "")
	if err != nil || len(ps) != 1 {
		t.Errorf("limit 0 -> %d passes, %v; want 1", len(ps), err)
	}
	ps, _ = l.Passes(context.Background(), 100000, time.Time{}, "")
	if len(ps) != 4 {
		t.Errorf("huge limit -> %d", len(ps))
	}
	s, err := l.Summary(context.Background(), 0)
	if err != nil || s.Totals.Passes != 3 {
		t.Errorf("days 0 -> %d passes, %v; want today's 3", s.Totals.Passes, err)
	}
	s, _ = l.Summary(context.Background(), 1000)
	if s.Totals.Passes != 4 {
		t.Errorf("days 1000 -> %d passes; want 90 days' 4", s.Totals.Passes)
	}
	if _, err := l.Disagreements(context.Background(), -1, -1); err != nil {
		t.Errorf("negative params: %v", err)
	}
}

func TestRunDrainsOnCancel(t *testing.T) {
	l, _ := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Queue before Run starts so the batches are only written by the drain
	// (or by the loop, whichever comes first; both must land).
	for i := 0; i < 10; i++ {
		l.Record([]tracker.PassRecord{rec("h", t0.Add(-time.Duration(i)*time.Minute))})
	}
	cancel()
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	ps, _ := l.Passes(context.Background(), 100, time.Time{}, "")
	if len(ps) != 10 {
		t.Errorf("passes = %d, want 10", len(ps))
	}
}

func TestRunWritesLive(t *testing.T) {
	l, _ := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	l.Record([]tracker.PassRecord{rec("live", t0)})
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps, err := l.Passes(context.Background(), 10, time.Time{}, "")
		if err != nil {
			t.Fatalf("read while writer runs: %v", err)
		}
		if len(ps) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("record never written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestRecordNeverBlocks(t *testing.T) {
	l, _ := openTest(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize*3; i++ {
			l.Record([]tracker.PassRecord{rec("h", t0)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked with no writer running")
	}
}

// API reads run beside the writer; without WAL and a busy timeout on every
// pooled connection they would fail with SQLITE_BUSY.
func TestPragmasOnEveryConnection(t *testing.T) {
	l, _ := openTest(t)
	ctx := context.Background()
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		c, err := l.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		var mode string
		var timeout int
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" || timeout != 5000 {
			t.Errorf("journal_mode=%s busy_timeout=%d", mode, timeout)
		}
		c.Close()
	}
}

func TestPassesCursorWithinOneTick(t *testing.T) {
	l, _ := openTest(t)
	at := t0.Add(-time.Minute)
	for _, h := range []string{"a", "b", "c"} {
		mustWrite(t, l, rec(h, at))
	}
	var seen []string
	before, beforeHex := time.Time{}, ""
	for range 3 {
		ps, err := l.Passes(context.Background(), 2, before, beforeHex)
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) == 0 {
			break
		}
		seen = append(seen, hexes(ps)...)
		last := ps[len(ps)-1]
		before, beforeHex = time.Unix(last.FirstSeen, 0), last.Hex
	}
	if len(seen) != 3 || seen[0] != "c" || seen[1] != "b" || seen[2] != "a" {
		t.Errorf("paging through one tick's passes must reach all of them once: %v", seen)
	}
}
