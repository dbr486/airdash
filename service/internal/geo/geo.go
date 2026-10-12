// Package geo has the two great-circle calculations the tracker needs.
// Spherical earth is accurate to well under 1% at the radii we care about.
package geo

import "math"

const earthRadiusKm = 6371.0088

func DistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	φ1, φ2 := rad(lat1), rad(lat2)
	dφ := rad(lat2 - lat1)
	dλ := rad(lon2 - lon1)
	a := math.Sin(dφ/2)*math.Sin(dφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	return 2 * earthRadiusKm * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func BearingDeg(lat1, lon1, lat2, lon2 float64) int {
	φ1, φ2 := rad(lat1), rad(lat2)
	dλ := rad(lon2 - lon1)
	y := math.Sin(dλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(dλ)
	deg := math.Atan2(y, x) * 180 / math.Pi
	b := int(math.Round(deg))
	return ((b % 360) + 360) % 360
}

func rad(deg float64) float64 { return deg * math.Pi / 180 }
