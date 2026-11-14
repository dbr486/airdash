package sourcelog

import (
	"context"
	"database/sql"
	"time"

	"airdash/internal/tracker"
)

const (
	maxDays     = 90
	maxLimit    = 500
	dayLayout   = "2006-01-02"
	passColumns = `hex, first_seen, last_seen, callsign, reg, type, airline, min_dist_km, alt_ft, nearest, board_status, board_route, adsbdb_status, adsbdb_route, hexdb_status, hexdb_route, aeroapi_status, aeroapi_route, shown_source, shown_route`
)

type Answer struct {
	Status string `json:"status"`
	Route  string `json:"route,omitempty"`
}

type Pass struct {
	Hex         string  `json:"hex"`
	Callsign    string  `json:"callsign"`
	Reg         string  `json:"reg"`
	Type        string  `json:"type"`
	Airline     string  `json:"airline"`
	FirstSeen   int64   `json:"first_seen"` // unix seconds
	LastSeen    int64   `json:"last_seen"`
	MinDistKm   float64 `json:"min_dist_km"`
	AltFt       int     `json:"alt_ft"`
	Nearest     bool    `json:"nearest"`
	Board       Answer  `json:"board"`
	ADSBDB      Answer  `json:"adsbdb"`
	HexDB       Answer  `json:"hexdb"`
	AeroAPI     Answer  `json:"aeroapi"`
	ShownSource string  `json:"shown_source"`
	ShownRoute  string  `json:"shown_route"`
}

type Agreement struct {
	Agree    int `json:"agree"`
	Disagree int `json:"disagree"`
}

type Day struct {
	Day       string                    `json:"day"` // UTC calendar day of first_seen
	Passes    int                       `json:"passes"`
	Sources   map[string]map[string]int `json:"sources"` // source -> status -> count
	Shown     map[string]int            `json:"shown"`   // shown_source -> count
	Agreement map[string]Agreement      `json:"agreement"`
}

// Summary lists only days that had passes, oldest first; Totals covers the
// whole range with an empty Day.
type Summary struct {
	Days   []Day `json:"days"`
	Totals Day   `json:"totals"`
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// since is the start of the UTC day days-1 before today, so days=1 is
// today so far.
func (l *Log) since(days int) time.Time {
	y, m, d := l.now().UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(clamp(days, 1, maxDays) - 1))
}

func newDay(day string) Day {
	return Day{
		Day: day,
		Sources: map[string]map[string]int{
			tracker.SourceBoard: {}, tracker.SourceADSBDB: {},
			tracker.SourceHexDB: {}, tracker.SourceAeroAPI: {},
		},
		Shown:     map[string]int{},
		Agreement: map[string]Agreement{tracker.SourceADSBDB: {}, tracker.SourceHexDB: {}},
	}
}

// reference is the answer adsbdb and hexdb are judged against: the board
// reads the airport's own schedule and AeroAPI is the paid source, so
// either is trusted over the free per-callsign databases.
func reference(p Pass) (string, bool) {
	if p.Board.Status == tracker.StatusHit && p.Board.Route != "" {
		return p.Board.Route, true
	}
	if p.AeroAPI.Status == tracker.StatusHit && p.AeroAPI.Route != "" {
		return p.AeroAPI.Route, true
	}
	return "", false
}

// answered counts an implausible route too: the source named a route, and
// whether it was the right one is exactly what the log is for.
func answered(a Answer) bool {
	return (a.Status == tracker.StatusHit || a.Status == tracker.StatusImplausible) && a.Route != ""
}

func disagrees(p Pass) bool {
	var first string
	for _, a := range []Answer{p.Board, p.ADSBDB, p.HexDB, p.AeroAPI} {
		if !answered(a) {
			continue
		}
		if first == "" {
			first = a.Route
		} else if a.Route != first {
			return true
		}
	}
	return false
}

// summarySQL counts passes per UTC day and combination of answers inside
// SQLite, so a 90-day summary reads a few hundred grouped rows instead of
// every pass. The agreement columns mirror reference() and answered():
// 0 = no reference or no answer, 1 = agrees, 2 = disagrees.
const summarySQL = `
WITH p AS (
  SELECT date(first_seen, 'unixepoch') AS d, board_status, adsbdb_status, adsbdb_route,
    hexdb_status, hexdb_route, aeroapi_status, shown_source,
    CASE WHEN board_status = 'hit' AND board_route != '' THEN board_route
         WHEN aeroapi_status = 'hit' AND aeroapi_route != '' THEN aeroapi_route
         ELSE '' END AS ref
  FROM passes WHERE first_seen >= ?)
SELECT d, board_status, adsbdb_status, hexdb_status, aeroapi_status, shown_source,
  CASE WHEN ref = '' OR adsbdb_status NOT IN ('hit', 'implausible') OR adsbdb_route = '' THEN 0
       WHEN adsbdb_route = ref THEN 1 ELSE 2 END,
  CASE WHEN ref = '' OR hexdb_status NOT IN ('hit', 'implausible') OR hexdb_route = '' THEN 0
       WHEN hexdb_route = ref THEN 1 ELSE 2 END,
  count(*)
FROM p GROUP BY 1, 2, 3, 4, 5, 6, 7, 8 ORDER BY 1`

func (l *Log) Summary(ctx context.Context, days int) (Summary, error) {
	rows, err := l.db.QueryContext(ctx, summarySQL, l.since(days).Unix())
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	s := Summary{Days: []Day{}, Totals: newDay("")}
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.day, &g.board, &g.adsbdb, &g.hexdb, &g.aeroapi, &g.shown, &g.adsbdbAgree, &g.hexdbAgree, &g.n); err != nil {
			return Summary{}, err
		}
		if n := len(s.Days); n == 0 || s.Days[n-1].Day != g.day {
			s.Days = append(s.Days, newDay(g.day))
		}
		s.Days[len(s.Days)-1].addGroup(g)
		s.Totals.addGroup(g)
	}
	return s, rows.Err()
}

type group struct {
	day                           string
	board, adsbdb, hexdb, aeroapi string
	shown                         string
	adsbdbAgree, hexdbAgree, n    int
}

func (d *Day) addGroup(g group) {
	d.Passes += g.n
	d.Sources[tracker.SourceBoard][g.board] += g.n
	d.Sources[tracker.SourceADSBDB][g.adsbdb] += g.n
	d.Sources[tracker.SourceHexDB][g.hexdb] += g.n
	d.Sources[tracker.SourceAeroAPI][g.aeroapi] += g.n
	d.Shown[g.shown] += g.n
	for src, v := range map[string]int{tracker.SourceADSBDB: g.adsbdbAgree, tracker.SourceHexDB: g.hexdbAgree} {
		a := d.Agreement[src]
		switch v {
		case 1:
			a.Agree += g.n
		case 2:
			a.Disagree += g.n
		}
		d.Agreement[src] = a
	}
}

// Passes returns passes before the cursor (before, beforeHex), newest first;
// a zero before means now. Aircraft first seen in the same tick share
// first_seen, so the cursor carries the hex too: a page ending inside such a
// group must not skip the rest of it.
func (l *Log) Passes(ctx context.Context, limit int, before time.Time, beforeHex string) ([]Pass, error) {
	if before.IsZero() {
		// Strictly after now, so a pass that started this second is listed.
		before, beforeHex = l.now().Add(time.Second), ""
	}
	limit = clamp(limit, 1, maxLimit)
	if beforeHex == "" {
		return l.query(ctx, `WHERE first_seen < ? ORDER BY first_seen DESC, hex DESC LIMIT ?`, before.Unix(), limit)
	}
	return l.query(ctx, `WHERE (first_seen < ? OR (first_seen = ? AND hex < ?)) ORDER BY first_seen DESC, hex DESC LIMIT ?`,
		before.Unix(), before.Unix(), beforeHex, limit)
}

// Disagreements returns, newest first, passes in the last days where two
// answering sources named different routes. The rule lives in disagrees(),
// so rows are streamed newest first and the scan stops at limit matches:
// memory stays bounded by limit, not by the size of the window.
func (l *Log) Disagreements(ctx context.Context, days, limit int) ([]Pass, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT `+passColumns+` FROM passes WHERE first_seen >= ? ORDER BY first_seen DESC, hex DESC`,
		l.since(days).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	limit = clamp(limit, 1, maxLimit)
	out := []Pass{}
	for len(out) < limit && rows.Next() {
		p, err := scanPass(rows)
		if err != nil {
			return nil, err
		}
		if disagrees(p) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func (l *Log) query(ctx context.Context, where string, args ...any) ([]Pass, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT `+passColumns+` FROM passes `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pass{}
	for rows.Next() {
		p, err := scanPass(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPass(rows *sql.Rows) (Pass, error) {
	var p Pass
	err := rows.Scan(&p.Hex, &p.FirstSeen, &p.LastSeen, &p.Callsign, &p.Reg, &p.Type, &p.Airline,
		&p.MinDistKm, &p.AltFt, &p.Nearest,
		&p.Board.Status, &p.Board.Route, &p.ADSBDB.Status, &p.ADSBDB.Route,
		&p.HexDB.Status, &p.HexDB.Route, &p.AeroAPI.Status, &p.AeroAPI.Route,
		&p.ShownSource, &p.ShownRoute)
	return p, err
}
