package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func serveFixture(t *testing.T, status int, file string, gotUA *string, gotPath *string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotUA != nil {
			*gotUA = r.Header.Get("User-Agent")
		}
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestADSBDB_KnownRoute(t *testing.T) {
	var ua, path string
	srv := serveFixture(t, 200, "adsbdb_baw466.json", &ua, &path)
	defer srv.Close()

	l := NewADSBDB(srv.Client(), srv.URL, "airdash/test")
	r, ok, err := l.Route(t.Context(), "BAW466")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected route to be known")
	}
	want := Route{Airline: "British Airways", AirlineIATA: "BA", OriginIATA: "LHR", OriginICAO: "EGLL", DestIATA: "BCN", DestICAO: "LEBL",
		OriginName: "London", DestName: "Barcelona"}
	if r != want {
		t.Errorf("got %+v, want %+v", r, want)
	}
	if ua != "airdash/test" {
		t.Errorf("User-Agent = %q", ua)
	}
	if path != "/v0/callsign/BAW466" {
		t.Errorf("path = %q", path)
	}
}

func TestADSBDB_UnknownCallsign(t *testing.T) {
	srv := serveFixture(t, 200, "adsbdb_unknown.json", nil, nil)
	defer srv.Close()

	l := NewADSBDB(srv.Client(), srv.URL, "airdash/test")
	r, ok, err := l.Route(t.Context(), "ZZZ999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || r != (Route{}) {
		t.Errorf("expected unknown, got ok=%v route=%+v", ok, r)
	}
}

func TestADSBDB_NotFoundStatusIsUnknown(t *testing.T) {
	srv := serveFixture(t, 404, "adsbdb_unknown.json", nil, nil)
	defer srv.Close()

	l := NewADSBDB(srv.Client(), srv.URL, "airdash/test")
	_, ok, err := l.Route(t.Context(), "ZZZ999")
	if err != nil || ok {
		t.Errorf("404 should be unknown without error, got ok=%v err=%v", ok, err)
	}
}

func TestADSBDB_ServerErrorIsError(t *testing.T) {
	srv := serveFixture(t, 503, "adsbdb_unknown.json", nil, nil)
	defer srv.Close()

	l := NewADSBDB(srv.Client(), srv.URL, "airdash/test")
	_, _, err := l.Route(t.Context(), "BAW466")
	if err == nil {
		t.Fatal("expected error on 503")
	}
}

func TestADSBDB_MalformedBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>"))
	}))
	defer srv.Close()

	l := NewADSBDB(srv.Client(), srv.URL, "airdash/test")
	_, _, err := l.Route(t.Context(), "BAW466")
	if err == nil {
		t.Fatal("expected error on malformed body")
	}
}

func TestDecodeADSBDB_TrimsMunicipality(t *testing.T) {
	body := []byte(`{"response":{"flightroute":{"airline":{"name":"X"},
		"origin":{"iata_code":"LHR","icao_code":"EGLL","municipality":"  London "},
		"destination":{"iata_code":"BCN","icao_code":"LEBL","municipality":""}}}}`)
	r, ok, err := decodeADSBDB(body)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.OriginName != "London" || r.DestName != "" {
		t.Errorf("got %+v", r)
	}
}

func TestASCIIFold(t *testing.T) {
	cases := map[string]string{
		"Málaga": "Malaga", "Zürich": "Zurich", "São Paulo": "Sao Paulo", "Kraków": "Krakow",
		"Bodø": "Bodo", "Łódź": "Lodz", "Reykjavík": "Reykjavik", "İstanbul": "Istanbul",
		"Düsseldorf": "Dusseldorf", "Ålesund": "Alesund", "London": "London", "Ærø": "AEro",
		"東京": "", "Timișoara": "Timisoara", "Iași": "Iasi", "東京 (Tokyo)": "", "Đà Nẵng": "",
		"Ma\u0301laga": "Malaga", "Ĳmuiden": "IJmuiden",
	}
	for in, want := range cases {
		if got := asciiFold(in); got != want {
			t.Errorf("asciiFold(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeADSBDB_FoldsMunicipality(t *testing.T) {
	body := []byte(`{"response":{"flightroute":{"airline":{"name":"X"},
		"origin":{"iata_code":"AGP","icao_code":"LEMG","municipality":"Málaga"},
		"destination":{"iata_code":"ZRH","icao_code":"LSZH","municipality":"Zürich"}}}}`)
	r, _, _ := decodeADSBDB(body)
	if r.OriginName != "Malaga" || r.DestName != "Zurich" {
		t.Errorf("got %+v", r)
	}
}

func TestDecodeADSBDB_RejectsOddAirlineIATA(t *testing.T) {
	for in, want := range map[string]string{"ba": "BA", " U2 ": "U2", "BAW": "", "B": "", "b/": "", "": ""} {
		body := []byte(`{"response":{"flightroute":{"airline":{"name":"X","iata":"` + in + `"},
			"origin":{"iata_code":"LHR","icao_code":"EGLL"},"destination":{"iata_code":"BCN","icao_code":"LEBL"}}}}`)
		r, _, _ := decodeADSBDB(body)
		if r.AirlineIATA != want {
			t.Errorf("iata %q -> %q, want %q", in, r.AirlineIATA, want)
		}
	}
}
