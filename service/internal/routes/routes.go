// Package routes resolves a callsign to its airline, origin and
// destination. The ADS-B feed itself carries none of that.
package routes

import "context"

type Route struct {
	Airline string
	// Two-letter IATA airline code, the key for airline logos.
	AirlineIATA string
	OriginIATA  string
	OriginICAO  string
	DestIATA    string
	DestICAO    string
	// Municipality (city) of each end, for the "show city names" display
	// option. Empty when adsbdb has none; callers fall back to the code.
	OriginName string
	DestName   string
}

// Lookup resolves one callsign. The bool distinguishes "definitely
// unknown" (cacheable) from an error (transient, must not be cached).
type Lookup interface {
	Route(ctx context.Context, callsign string) (Route, bool, error)
}
