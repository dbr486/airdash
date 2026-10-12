// Package board turns an airport's departures and arrivals board into
// delay status for the aircraft overhead, so one cheap download replaces
// a paid lookup per flight.
package board

import "time"

// Row is one board line. Times are UTC and zero when the board has none;
// delay minutes are nil when the board gives none (early departures often
// come back null and are derived from the times instead).
type Row struct {
	FlightIATA, FlightICAO, AirlineIATA string
	DepIATA, DepICAO, ArrIATA, ArrICAO  string
	DepSched, DepEst, DepActual         time.Time
	ArrSched, ArrEst, ArrActual         time.Time
	DepDelayMin, ArrDelayMin            *int
	Status                              string
}
