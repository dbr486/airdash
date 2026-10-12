package filter

import (
	"testing"

	"airdash/internal/routes"
)

var lhrBcn = routes.Route{OriginIATA: "LHR", OriginICAO: "EGLL", DestIATA: "BCN", DestICAO: "LEBL"}
var bcnLhr = routes.Route{OriginIATA: "BCN", OriginICAO: "LEBL", DestIATA: "LHR", DestICAO: "EGLL"}
var lgwAms = routes.Route{OriginIATA: "LGW", OriginICAO: "EGKK", DestIATA: "AMS", DestICAO: "EHAM"}

func TestParse_EmptyIsInactive(t *testing.T) {
	f, err := Parse("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Active() {
		t.Error("empty filter must be inactive")
	}
	if !f.Match(lhrBcn) || !f.Match(routes.Route{}) {
		t.Error("inactive filter must match everything")
	}
}

func TestParse_RejectsBadCodes(t *testing.T) {
	for _, in := range []string{"LH", "LONDON", "L1R", "LHR,,", "LHR;LGW"} {
		if _, err := Parse(in, "", ""); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name                  string
		origin, dest, airport string
		route                 routes.Route
		want                  bool
	}{
		{"origin iata", "LHR", "", "", lhrBcn, true},
		{"origin icao", "EGLL", "", "", lhrBcn, true},
		{"origin lowercase and spaces", " lhr , lgw ", "", "", lgwAms, true},
		{"origin mismatch", "LHR", "", "", bcnLhr, false},
		{"dest iata", "", "BCN", "", lhrBcn, true},
		{"dest mismatch", "", "BCN", "", bcnLhr, false},
		{"origin AND dest both match", "LHR", "BCN", "", lhrBcn, true},
		{"origin AND dest only one matches", "LHR", "AMS", "", lhrBcn, false},
		{"airport either end, origin side", "", "", "LHR", lhrBcn, true},
		{"airport either end, dest side", "", "", "LHR", bcnLhr, true},
		{"airport neither end", "", "", "LHR", lgwAms, false},
		{"airport AND dest", "", "LHR", "BCN", bcnLhr, true},
		{"airport AND dest fails on dest", "", "AMS", "BCN", bcnLhr, false},
		{"empty route never matches an active filter", "LHR", "", "", routes.Route{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(tc.origin, tc.dest, tc.airport)
			if err != nil {
				t.Fatal(err)
			}
			if !f.Active() {
				t.Fatal("expected active filter")
			}
			if got := f.Match(tc.route); got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}
