// Package flightstatus answers "is this flight late?" for the aircraft on
// the display. FlightAware AeroAPI resolves an ADS-B callsign to the
// commercial flight and reports schedule vs. estimate; this package picks
// the instance that is airborne now and reduces it to one small Status.
package flightstatus

import (
	"context"
	"time"
)

type Kind string

const (
	KindOnTime    Kind = "on_time"
	KindLate      Kind = "late"
	KindEarly     Kind = "early"
	KindCancelled Kind = "cancelled"
	KindDiverted  Kind = "diverted"
)

type Status struct {
	Kind     Kind   `json:"kind"`
	DelayMin int    `json:"min"`
	Basis    string `json:"basis,omitempty"` // empty for cancelled/diverted, which have no delay basis
}

// Query identifies the aircraft to look up. AeroAPI only needs the
// callsign; the airport board also uses the airline and the route.
type Query struct {
	Callsign    string
	AirlineIATA string
	Origin      string
	Dest        string
	// Seen is when this aircraft was first observed airborne in range (zero
	// when unknown). A departure seen in the air must have left the gate a
	// few minutes earlier, which dates a departure the board has not
	// recorded yet.
	Seen time.Time
}

// Lookup resolves one aircraft. (status, true, nil) when known;
// (Status{}, false, nil) when the flight is unknown or has no schedule
// data (cacheable); err for transient failures (never cached as a result).
type Lookup interface {
	Status(ctx context.Context, q Query) (Status, bool, error)
}

// onTimeWindowMin is the |delay| below which a flight counts as on time.
const onTimeWindowMin = 5

// Classify maps a signed delay in minutes to a Status using the on-time
// window shared by every source.
func Classify(delayMin int, basis string) Status {
	st := Status{DelayMin: delayMin, Basis: basis}
	switch {
	case delayMin >= onTimeWindowMin:
		st.Kind = KindLate
	case delayMin <= -onTimeWindowMin:
		st.Kind = KindEarly
	default:
		st.Kind = KindOnTime
	}
	return st
}
