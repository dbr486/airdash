package geo

import (
	"math"
	"testing"
)

func TestDistanceKm(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		want, tol              float64
	}{
		{"same point", 51.47, -0.45, 51.47, -0.45, 0, 0.0001},
		{"heathrow to gatwick", 51.4706, -0.4619, 51.1537, -0.1821, 40.2, 0.5},
		{"one degree of latitude", 0, 0, 1, 0, 111.19, 0.1},
		{"across the antimeridian", 0, 179.5, 0, -179.5, 111.19, 0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DistanceKm(tc.lat1, tc.lon1, tc.lat2, tc.lon2)
			if math.Abs(got-tc.want) > tc.tol {
				t.Errorf("got %.3f, want %.3f ±%.3f", got, tc.want, tc.tol)
			}
		})
	}
}

func TestBearingDeg(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		want                   int
	}{
		{"due north", 0, 0, 1, 0, 0},
		{"due east", 0, 0, 0, 1, 90},
		{"due south", 1, 0, 0, 0, 180},
		{"due west", 0, 1, 0, 0, 270},
		{"north-west wraps to 315 not -45", 0, 0, 1, -1, 315},
		{"heathrow to gatwick roughly south-east", 51.4706, -0.4619, 51.1537, -0.1821, 151},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BearingDeg(tc.lat1, tc.lon1, tc.lat2, tc.lon2)
			if got < 0 || got > 359 {
				t.Fatalf("bearing %d outside 0..359", got)
			}
			if d := got - tc.want; d < -1 || d > 1 {
				t.Errorf("got %d, want %d ±1", got, tc.want)
			}
		})
	}
}
