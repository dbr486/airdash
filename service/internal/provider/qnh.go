package provider

import (
	"math"
	"sort"
)

// Barometric altitude is reported against the 1013.25 hPa standard
// atmosphere, so on a high- or low-pressure day it reads low or high
// compared with the height people expect near the ground. Below the
// transition altitude (6,000 ft at Málaga) crews fly local QNH, so we
// apply the same correction; above it, flight levels stay as reported.
const (
	transitionAltFt = 6000
	ftPerHPa        = 27.0
	stdQNH          = 1013.25
	minQNH          = 950.0 // outside this range nav_qnh is a bogus or unset value
	maxQNH          = 1050.0
)

func validQNH(q float64) bool { return q >= minQNH && q <= maxQNH }

func correctQNH(altFt int, qnh float64) int {
	v := int(math.Round(float64(altFt) + (qnh-stdQNH)*ftPerHPa))
	if v < 0 {
		return 0
	}
	return v
}

// medianQNH returns the median of the valid values, or false if none.
// The median resists the odd aircraft with a stale selected pressure.
func medianQNH(vals []float64) (float64, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	sort.Float64s(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2], true
	}
	return (vals[n/2-1] + vals[n/2]) / 2, true
}
