// Package sourcelog records, per aircraft pass, what each route source
// answered and which route went on the screen, in a local SQLite file, so
// the source order and the paid cap can be judged from real traffic.
package sourcelog

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"airdash/internal/capture"
	"airdash/internal/tracker"

	// Pure-Go driver: the image is built with CGO off on scratch.
	_ "modernc.org/sqlite"
)

const (
	retention     = 90 * 24 * time.Hour
	pruneInterval = time.Hour
	queueSize     = 64
	// The paid AirLabs board is queried per airport and its answers go stale
	// fast; a week is enough to debug it without paying for storage.
	boardRetention = 7 * 24 * time.Hour
)

const schema = `
CREATE TABLE IF NOT EXISTS passes (
	hex            TEXT    NOT NULL,
	first_seen     INTEGER NOT NULL,
	last_seen      INTEGER NOT NULL,
	callsign       TEXT    NOT NULL DEFAULT '',
	reg            TEXT    NOT NULL DEFAULT '',
	type           TEXT    NOT NULL DEFAULT '',
	airline        TEXT    NOT NULL DEFAULT '',
	min_dist_km    REAL    NOT NULL,
	alt_ft         INTEGER NOT NULL,
	nearest        INTEGER NOT NULL DEFAULT 0,
	board_status   TEXT    NOT NULL,
	board_route    TEXT    NOT NULL DEFAULT '',
	adsbdb_status  TEXT    NOT NULL,
	adsbdb_route   TEXT    NOT NULL DEFAULT '',
	hexdb_status   TEXT    NOT NULL,
	hexdb_route    TEXT    NOT NULL DEFAULT '',
	aeroapi_status TEXT    NOT NULL,
	aeroapi_route  TEXT    NOT NULL DEFAULT '',
	shown_source   TEXT    NOT NULL,
	shown_route    TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (hex, first_seen)
);
CREATE INDEX IF NOT EXISTS passes_first_seen ON passes (first_seen);
CREATE INDEX IF NOT EXISTS passes_callsign ON passes (callsign);

-- body is gzip-compressed; size is the uncompressed length.
CREATE TABLE IF NOT EXISTS responses (
	id           INTEGER PRIMARY KEY,
	source       TEXT    NOT NULL,
	subject      TEXT    NOT NULL DEFAULT '',
	fetched_at   INTEGER NOT NULL,
	url          TEXT    NOT NULL,
	status       INTEGER NOT NULL,
	content_type TEXT    NOT NULL DEFAULT '',
	size         INTEGER NOT NULL,
	body         BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS responses_lookup ON responses (source, subject, fetched_at);
CREATE INDEX IF NOT EXISTS responses_fetched ON responses (fetched_at);
`

// keepHit keeps a stored hit (status and route together) against a later
// non-hit, so a row holds the best answer the source gave this pass; any
// other answer is replaced by the latest.
func keepHit(src string) string {
	s, r := src+"_status", src+"_route"
	return fmt.Sprintf(`%[1]s = CASE WHEN %[1]s = 'hit' AND excluded.%[1]s <> 'hit' THEN %[1]s ELSE excluded.%[1]s END,
	%[2]s = CASE WHEN %[1]s = 'hit' AND excluded.%[1]s <> 'hit' THEN %[2]s ELSE excluded.%[2]s END`, s, r)
}

// The CASE expressions in SET read the row's old values: SQLite evaluates
// every SET expression against the row before the update, so the status
// test for the route column still sees the stored status.
var upsert = `
INSERT INTO passes (hex, first_seen, last_seen, callsign, reg, type, airline,
	min_dist_km, alt_ft, nearest,
	board_status, board_route, adsbdb_status, adsbdb_route,
	hexdb_status, hexdb_route, aeroapi_status, aeroapi_route,
	shown_source, shown_route)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (hex, first_seen) DO UPDATE SET
	last_seen   = MAX(last_seen, excluded.last_seen),
	callsign    = CASE WHEN excluded.callsign <> '' THEN excluded.callsign ELSE callsign END,
	reg         = CASE WHEN excluded.reg <> '' THEN excluded.reg ELSE reg END,
	type        = CASE WHEN excluded.type <> '' THEN excluded.type ELSE type END,
	airline     = CASE WHEN excluded.airline <> '' THEN excluded.airline ELSE airline END,
	min_dist_km = MIN(min_dist_km, excluded.min_dist_km),
	alt_ft      = MIN(alt_ft, excluded.alt_ft),
	nearest     = MAX(nearest, excluded.nearest),
	` + keepHit("board") + `,
	` + keepHit("adsbdb") + `,
	` + keepHit("hexdb") + `,
	` + keepHit("aeroapi") + `,
	shown_route  = CASE WHEN excluded.shown_source <> 'none' THEN excluded.shown_route ELSE shown_route END,
	shown_source = CASE WHEN excluded.shown_source <> 'none' THEN excluded.shown_source ELSE shown_source END
`

// Log is a tracker.Recorder backed by SQLite. Record only enqueues; Run
// owns every write, so the tick path never waits on disk and writes never
// contend with each other.
type Log struct {
	db     *sql.DB
	now    func() time.Time
	logger *slog.Logger
	queue  chan []tracker.PassRecord
	// responses is separate from queue so a burst of large captures cannot
	// crowd out pass records.
	responses chan capture.Response
}

var (
	_ tracker.Recorder = (*Log)(nil)
	_ capture.Sink     = (*Log)(nil)
)

func Open(path string, now func() time.Time, logger *slog.Logger) (*Log, error) {
	// The pragmas go in the DSN so every pooled connection gets them, not
	// just the first. WAL lets API reads run while the writer commits, and
	// the busy timeout makes the rare lock overlap (checkpoints, prune) wait
	// instead of failing with SQLITE_BUSY. Immediate transactions take the
	// write lock up front, so a transaction never fails half-way on upgrade.
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sourcelog: open %s: %w", path, err)
	}
	// One writer plus a few concurrent API readers.
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sourcelog: schema: %w", err)
	}
	return &Log{db: db, now: now, logger: logger, queue: make(chan []tracker.PassRecord, queueSize),
		responses: make(chan capture.Response, queueSize)}, nil
}

// Record hands a tick's batch to the writer. It never blocks: a full queue
// means the disk is behind, and losing a tick of history is better than
// stalling the display.
func (l *Log) Record(recs []tracker.PassRecord) {
	if len(recs) == 0 {
		return
	}
	select {
	case l.queue <- recs:
	default:
		l.logger.Debug("source log queue full; batch dropped", "records", len(recs))
	}
}

// Capture queues an upstream response for storage. It never blocks: it runs
// on the lookup's goroutine, and a missing raw copy is cheaper than a slow
// lookup.
func (l *Log) Capture(r capture.Response) {
	select {
	case l.responses <- r:
	default:
		l.logger.Debug("source log response queue full; capture dropped", "source", r.Source, "subject", r.Subject)
	}
}

// Run writes queued batches until ctx is done, then writes whatever is
// still queued and returns, so a clean shutdown loses nothing. Call Close
// only after Run has returned.
func (l *Log) Run(ctx context.Context) {
	l.pruneLogged()
	tick := time.NewTicker(pruneInterval)
	defer tick.Stop()
	for {
		select {
		case recs := <-l.queue:
			l.writeLogged(recs)
		case r := <-l.responses:
			l.writeResponseLogged(r)
		case <-tick.C:
			l.pruneLogged()
		case <-ctx.Done():
			for {
				select {
				case recs := <-l.queue:
					l.writeLogged(recs)
				case r := <-l.responses:
					l.writeResponseLogged(r)
				default:
					return
				}
			}
		}
	}
}

func (l *Log) Close() error { return l.db.Close() }

func (l *Log) writeLogged(recs []tracker.PassRecord) {
	if err := l.write(recs); err != nil {
		l.logger.Warn("source log write failed", "records", len(recs), "err", err)
	}
}

func (l *Log) writeResponseLogged(r capture.Response) {
	if err := l.writeResponse(r); err != nil {
		l.logger.Warn("source log response write failed", "source", r.Source, "subject", r.Subject, "err", err)
	}
}

func (l *Log) pruneLogged() {
	if err := l.prune(); err != nil {
		l.logger.Warn("source log prune failed", "err", err)
	}
}

// write upserts one batch in a single transaction: one fsync per tick
// rather than per aircraft.
func (l *Log) write(recs []tracker.PassRecord) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(upsert)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range recs {
		shown := r.ShownSource
		if shown == "" {
			shown = tracker.SourceNone
		}
		nearest := 0
		if r.Nearest {
			nearest = 1
		}
		if _, err := stmt.Exec(r.Hex, r.FirstSeen.Unix(), r.LastSeen.Unix(),
			r.Callsign, r.Reg, r.Type, r.Airline,
			r.DistKm, r.AltFt, nearest,
			r.Board.Status, r.Board.Route, r.ADSBDB.Status, r.ADSBDB.Route,
			r.HexDB.Status, r.HexDB.Route, r.AeroAPI.Status, r.AeroAPI.Route,
			shown, r.ShownRoute); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// writeResponse stores the body gzipped: route JSON is highly repetitive and
// the log holds months of it. BestSpeed keeps the single writer cheap.
func (l *Log) writeResponse(r capture.Response) error {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if _, err := zw.Write(r.Body); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err = l.db.Exec(`INSERT INTO responses (source, subject, fetched_at, url, status, content_type, size, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Source, r.Subject, r.FetchedAt.Unix(), r.URL, r.Status, r.ContentType, len(r.Body), buf.Bytes())
	return err
}

func (l *Log) prune() error {
	now := l.now()
	if _, err := l.db.Exec(`DELETE FROM passes WHERE first_seen < ?`, now.Add(-retention).Unix()); err != nil {
		return err
	}
	if _, err := l.db.Exec(`DELETE FROM responses WHERE source = ? AND fetched_at < ?`, capture.SourceAirLabs, now.Add(-boardRetention).Unix()); err != nil {
		return err
	}
	_, err := l.db.Exec(`DELETE FROM responses WHERE fetched_at < ?`, now.Add(-retention).Unix())
	return err
}
