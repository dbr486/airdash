package board

import (
	"strings"
	"time"

	"airdash/internal/flightstatus"
)

const (
	depBefore   = 45 * time.Minute // a recorded actual departure this old is another aircraft
	depLateMax  = 3 * time.Hour    // a departure the board has not recorded yet may be this late
	depAfter    = 10 * time.Minute
	arrBefore   = 10 * time.Minute
	arrAfter    = 60 * time.Minute
	callsignMax = 6 * time.Hour // a callsign row further from now than this is another day's flight
	landedGrace = 3 * time.Minute
	// Gate departure to wheels-up: the allowance between the board's
	// scheduled departure (off-block) and our first sighting airborne.
	takeoffAllowance = 10 * time.Minute
)

// Match finds the board row for an aircraft. The transponder callsign is
// authoritative when it equals a row's ICAO flight id (preferring the row
// whose direction agrees with the route, within a loose time window,
// since tag-on flights reuse one number). Ryanair and easyJet fly
// scrambled callsigns, so the fallback is the same airline and far
// airport with a departure/arrival time near now. Rows are ranked so an
// aircraft seen airborne prefers a departure that has actually left and
// an arrival that has not yet landed; when two rows are equally plausible
// the match fails closed so a paid, exact source can take over.
func Match(rows []Row, airport string, q flightstatus.Query, now time.Time) (Row, bool) {
	codes := airportCodes(rows, airport)
	arrival, dirKnown := isArrival(codes, q)

	if cs := strings.ToUpper(strings.TrimSpace(q.Callsign)); cs != "" {
		var best Row
		var bestDist time.Duration
		found := false
		for _, r := range rows {
			if r.FlightICAO != cs {
				continue
			}
			rowArr := sameAirportAny(codes, r.ArrIATA, r.ArrICAO)
			if dirKnown && rowArr != arrival {
				continue
			}
			ref := first(r.DepActual, r.DepEst, r.DepSched)
			if rowArr {
				ref = first(r.ArrActual, r.ArrEst, r.ArrSched)
			}
			d := abs(now.Sub(ref))
			if ref.IsZero() || d > callsignMax {
				continue
			}
			if !found || d < bestDist {
				best, bestDist, found = r, d, true
			}
		}
		if found {
			return best, true
		}
	}

	if !dirKnown || q.AirlineIATA == "" {
		return Row{}, false
	}
	far := q.Dest
	if arrival {
		far = q.Origin
	}
	type cand struct {
		r    Row
		tier int
		d    time.Duration
	}
	var cands []cand
	for _, r := range rows {
		if r.AirlineIATA != strings.ToUpper(q.AirlineIATA) {
			continue
		}
		var ref time.Time
		tier := 0
		if arrival {
			if !sameAirport(far, r.DepIATA, r.DepICAO) {
				continue
			}
			ref = first(r.ArrEst, r.ArrSched)
			if !r.ArrActual.IsZero() && now.Sub(r.ArrActual) > landedGrace {
				tier = 1 // landed a while ago: the aircraft overhead is more likely the next one
			}
		} else {
			if !sameAirport(far, r.ArrIATA, r.ArrICAO) {
				continue
			}
			if r.DepActual.IsZero() || r.DepActual.After(now) {
				tier = 1 // not off the ground yet per the board: cannot be the one airborne
			}
			if r.DepActual.IsZero() {
				// No recorded departure: the board's estimate may be stale in either
				// direction, so the schedule decides, up to depLateMax late, and a
				// fresh-looking estimate is accepted too.
				ds := now.Sub(r.DepSched)
				de := now.Sub(first(r.DepEst, r.DepSched))
				okSched := !r.DepSched.IsZero() && ds <= depLateMax && ds >= -depAfter
				okEst := !r.DepEst.IsZero() && de <= depBefore && de >= -depAfter
				if !okSched && !okEst {
					continue
				}
				d := abs(ds)
				if okEst && abs(de) < d {
					d = abs(de)
				}
				cands = append(cands, cand{r: r, tier: tier, d: d})
				continue
			}
			ref = r.DepActual
		}
		if ref.IsZero() {
			continue
		}
		d := now.Sub(ref) // positive when ref is in the past
		if arrival {
			if d > arrBefore || d < -arrAfter {
				continue
			}
		} else if d > depBefore || d < -depAfter {
			continue
		}
		cands = append(cands, cand{r: r, tier: tier, d: abs(d)})
	}
	if len(cands) == 0 {
		return Row{}, false
	}
	best := cands[0]
	bestTierCount := 0
	for _, c := range cands {
		if c.tier < best.tier || (c.tier == best.tier && c.d < best.d) {
			best = c
		}
	}
	for _, c := range cands {
		if c.tier == best.tier {
			bestTierCount++
		}
	}
	if bestTierCount > 1 {
		return Row{}, false
	}
	return best.r, true
}

// airportCodes returns the board airport plus its twin code (IATA↔ICAO)
// learned from the rows, so a board configured as LEMG still matches a
// route that names AGP.
func airportCodes(rows []Row, airport string) []string {
	codes := []string{airport}
	for _, r := range rows {
		switch {
		case strings.EqualFold(r.DepIATA, airport):
			return append(codes, r.DepICAO)
		case strings.EqualFold(r.DepICAO, airport):
			return append(codes, r.DepIATA)
		case strings.EqualFold(r.ArrIATA, airport):
			return append(codes, r.ArrICAO)
		case strings.EqualFold(r.ArrICAO, airport):
			return append(codes, r.ArrIATA)
		}
	}
	return codes
}

// isArrival says which end of the route is the board airport.
func isArrival(codes []string, q flightstatus.Query) (bool, bool) {
	switch {
	case sameAirportAny(codes, q.Origin, ""):
		return false, true
	case sameAirportAny(codes, q.Dest, ""):
		return true, true
	}
	return false, false
}

func sameAirportAny(codes []string, iata, icao string) bool {
	for _, c := range codes {
		if sameAirport(c, iata, icao) {
			return true
		}
	}
	return false
}

func sameAirport(code, iata, icao string) bool {
	return code != "" && (strings.EqualFold(code, iata) || strings.EqualFold(code, icao))
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func first(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// StatusOf reduces a row to the display's status. Arrivals use the
// board's minutes, else estimated minus scheduled. Departures use the
// board's recorded actual when it has one; otherwise the aircraft we can
// see airborne is better evidence than a stale estimate: it left the gate
// about takeoffAllowance before it was first seen.
func StatusOf(r Row, arrival bool, seen time.Time) flightstatus.Status {
	if r.Status == "cancelled" {
		return flightstatus.Status{Kind: flightstatus.KindCancelled}
	}
	if arrival {
		return flightstatus.Classify(minutes(r.ArrDelayMin, r.ArrSched, first(r.ArrActual, r.ArrEst)), "arrival")
	}
	if r.DepActual.IsZero() && !seen.IsZero() && !r.DepSched.IsZero() {
		est := seen.Add(-takeoffAllowance)
		return flightstatus.Classify(int(est.Sub(r.DepSched).Round(time.Minute)/time.Minute), "departure")
	}
	return flightstatus.Classify(minutes(r.DepDelayMin, r.DepSched, first(r.DepActual, r.DepEst)), "departure")
}

func minutes(given *int, sched, est time.Time) int {
	if given != nil {
		return *given
	}
	if sched.IsZero() || est.IsZero() {
		return 0
	}
	return int(est.Sub(sched).Round(time.Minute) / time.Minute)
}
