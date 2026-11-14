package routes

import "airdash/internal/geo"

// adsbdb routes are crowd-sourced and keyed by callsign alone, and airlines
// reuse flight numbers on other city pairs, so a stale route can be confidently
// wrong. An aircraft actually flying a route lies inside an ellipse with the two
// airports as foci: the detour through its position may exceed the direct
// distance by detourFactor plus detourSlackKm (airways, vectoring, holds).
const (
	detourFactor  = 1.25
	detourSlackKm = 100
)

// Plausible reports whether an aircraft at lat/lon could be flying r. A route
// without airport positions cannot be judged and is taken as plausible.
func (r Route) Plausible(lat, lon float64) bool {
	if !r.HasCoords {
		return true
	}
	direct := geo.DistanceKm(r.OriginLat, r.OriginLon, r.DestLat, r.DestLon)
	via := geo.DistanceKm(r.OriginLat, r.OriginLon, lat, lon) + geo.DistanceKm(lat, lon, r.DestLat, r.DestLon)
	return via <= direct*detourFactor+detourSlackKm
}
