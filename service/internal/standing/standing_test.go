package standing

import "testing"

func TestLookupAirline(t *testing.T) {
	cases := map[string]Airline{
		"SCR": {Name: "Silver Cloud Air", IATA: ""},
		"EXS": {Name: "Jet2.com", IATA: "LS"},
		"vlg": {Name: "Vueling Airlines", IATA: "VY"},
	}
	for code, want := range cases {
		got, ok := LookupAirline(code)
		if !ok || got != want {
			t.Errorf("%s: got %+v %v, want %+v", code, got, ok, want)
		}
	}
	if _, ok := LookupAirline("ZZZ9"); ok {
		t.Error("ZZZ9 should be unknown")
	}
}

func TestCallsignPrefix(t *testing.T) {
	ok := map[string]string{"SCR137": "SCR", "EXS6M": "EXS", "RYR1AB": "RYR", "vlg1234": "VLG"}
	for cs, want := range ok {
		if got, found := CallsignPrefix(cs); !found || got != want {
			t.Errorf("%s: got %q %v", cs, got, found)
		}
	}
	for _, cs := range []string{"", "N123AB", "ECMKO", "GABCD", "D-CAFE", "SC1", "AB"} {
		if _, found := CallsignPrefix(cs); found {
			t.Errorf("%s: should not match", cs)
		}
	}
}

func TestTypeName(t *testing.T) {
	want := map[string]string{"C56X": "Citation XLS", "B738": "737-800", "A320": "A320", "A20N": "A320neo", "E195": "Embraer 195", "ZZZZ": ""}
	for code, w := range want {
		if got := TypeName(code); got != w {
			t.Errorf("%s: got %q want %q", code, got, w)
		}
	}
	for code, name := range typeNames {
		if len(name) > 14 {
			t.Errorf("%s: %q longer than 14", code, name)
		}
	}
}
