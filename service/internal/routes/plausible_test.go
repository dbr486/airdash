package routes

import "testing"

func TestRoute_Plausible(t *testing.T) {
	// LGW -> BUD, the stale adsbdb route a reused Wizz Air flight number carried.
	lgwBud := Route{HasCoords: true, OriginLat: 51.1481, OriginLon: -0.19028, DestLat: 47.42976, DestLon: 19.26109}
	cases := []struct {
		name     string
		r        Route
		lat, lon float64
		want     bool
	}{
		{"climbing out of the origin", lgwBud, 51.2, -0.1, true},
		{"midway along the route", lgwBud, 49.8, 9.5, true},
		{"on approach to the destination", lgwBud, 47.5, 19.0, true},
		{"over Malaga, nowhere near the route", lgwBud, 36.65, -4.48, false},
		{"no coordinates cannot be judged", Route{OriginIATA: "LGW", DestIATA: "BUD"}, 36.65, -4.48, true},
	}
	for _, c := range cases {
		if got := c.r.Plausible(c.lat, c.lon); got != c.want {
			t.Errorf("%s: Plausible = %v, want %v", c.name, got, c.want)
		}
	}
}
