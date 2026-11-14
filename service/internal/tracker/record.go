package tracker

import "time"

// Source answer statuses for the route source log. They are stored as text,
// so the values are part of the log's on-disk format.
const (
	StatusHit         = "hit"         // a route the aircraft could be flying
	StatusImplausible = "implausible" // a route, but the position rules it out
	StatusUnknown     = "unknown"     // the source has no (usable) route
	StatusMiss        = "miss"        // board only: no row for the leg flying now
	StatusError       = "error"
	StatusSkipped     = "skipped"   // this tick's lookup budget was spent
	StatusCooldown    = "cooldown"  // held back after an earlier failure or a rate-limit ban
	StatusOff         = "off"       // source not configured or switched off
	StatusNone        = "none"      // no callsign to ask with
	StatusNotAsked    = "not_asked" // not called: AeroAPI this pass, or hexdb with no log
)

// Route sources, as recorded in shown_source.
const (
	SourceBoard   = "board"
	SourceADSBDB  = "adsbdb"
	SourceHexDB   = "hexdb"
	SourceAeroAPI = "aeroapi"
	SourceNone    = "none"
)

// SourceAnswer is what one route source said about an aircraft. Route is
// "ORIG-DEST", each end IATA else ICAO; empty without a route.
type SourceAnswer struct {
	Status string
	Route  string
}

// PassRecord is one tick's view of an aircraft in range, keyed by its pass
// (Hex, FirstSeen). A recorder merges successive records of the same pass.
type PassRecord struct {
	Hex, Callsign, Reg, Type, Airline string
	FirstSeen, LastSeen               time.Time
	DistKm                            float64
	AltFt                             int
	Nearest                           bool

	Board, ADSBDB, HexDB, AeroAPI SourceAnswer

	ShownSource string
	ShownRoute  string
}

// Recorder receives every tick's records. It must not block: it runs on the
// tick path, ahead of publishing positions.
type Recorder interface {
	Record(recs []PassRecord)
}
