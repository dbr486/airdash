package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func serve(t *testing.T, file string, gotPath *string, gotUA *string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		if gotUA != nil {
			*gotUA = r.Header.Get("User-Agent")
		}
		_, _ = w.Write(body)
	}))
}

func TestDecodeReadsb_ADSBLolFields(t *testing.T) {
	var path, ua string
	srv := serve(t, "adsblol_point.json", &path, &ua)
	defer srv.Close()

	p := NewADSBLol(srv.Client(), srv.URL, "airdash/test")
	acs, err := p.Fetch(t.Context(), 51.47, -0.45, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(acs) != 4 {
		t.Fatalf("got %d aircraft, want 4", len(acs))
	}
	// 10 km = 5.4 nm, rounded up to 6.
	if path != "/v2/point/51.47/-0.45/6" {
		t.Errorf("path = %q", path)
	}
	if ua != "airdash/test" {
		t.Errorf("User-Agent = %q", ua)
	}
	ryr := acs[0]
	if ryr.Hex != "4cadb8" || ryr.Callsign != "RYR24LW" || ryr.Reg != "EI-IFX" || ryr.Type != "B38M" {
		t.Errorf("identity fields wrong: %+v", ryr)
	}
	if ryr.AltFt != 23650 || ryr.GroundKt != 390 || ryr.Track != 153 || ryr.OnGround {
		t.Errorf("numeric fields wrong: %+v", ryr)
	}
	if !ryr.HasPos || ryr.Lat != 51.446833 || ryr.Lon != -0.680237 {
		t.Errorf("position wrong: %+v", ryr)
	}
}

func TestDecodeReadsb_GroundAltitude(t *testing.T) {
	srv := serve(t, "adsblol_point.json", nil, nil)
	defer srv.Close()
	acs, err := NewADSBLol(srv.Client(), srv.URL, "airdash/test").Fetch(t.Context(), 51.47, -0.45, 10)
	if err != nil {
		t.Fatal(err)
	}
	ezy := acs[2]
	if ezy.Callsign != "EZY12AB" {
		t.Fatalf("expected EZY12AB at index 2, got %+v", ezy)
	}
	if !ezy.OnGround {
		t.Error(`alt_baro "ground" must set OnGround`)
	}
	if ezy.AltFt != 0 {
		t.Errorf("AltFt on ground should be 0, got %d", ezy.AltFt)
	}
}

func TestDecodeReadsb_MissingPositionAndCallsign(t *testing.T) {
	srv := serve(t, "adsblol_point.json", nil, nil)
	defer srv.Close()
	acs, err := NewADSBLol(srv.Client(), srv.URL, "airdash/test").Fetch(t.Context(), 51.47, -0.45, 10)
	if err != nil {
		t.Fatal(err)
	}
	c172 := acs[3]
	if c172.HasPos {
		t.Error("aircraft without lat/lon must have HasPos=false")
	}
	if c172.Callsign != "" {
		t.Errorf("missing flight must give empty Callsign, got %q", c172.Callsign)
	}
	if c172.Reg != "D-ABCD" {
		t.Errorf("Reg = %q", c172.Reg)
	}
}

func TestDecodeReadsb_ADSBFiShape(t *testing.T) {
	var path string
	srv := serve(t, "adsbfi_point.json", &path, nil)
	defer srv.Close()
	acs, err := NewADSBFi(srv.Client(), srv.URL, "airdash/test").Fetch(t.Context(), 51.47, -0.45, 1.852)
	if err != nil {
		t.Fatal(err)
	}
	if len(acs) != 1 || acs[0].Callsign != "RYR24LW" {
		t.Errorf("got %+v", acs)
	}
	// exactly 1 nm stays 1 nm
	if path != "/api/v2/lat/51.47/lon/-0.45/dist/1" {
		t.Errorf("path = %q", path)
	}
}

func TestDecodeReadsb_MissingListIsError(t *testing.T) {
	_, err := decodeReadsb([]byte(`{"error":"nope"}`))
	if err == nil {
		t.Fatal("expected error when body has neither ac nor aircraft")
	}
}

func TestDecodeReadsb_EmptyListIsOK(t *testing.T) {
	acs, err := decodeReadsb([]byte(`{"ac":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(acs) != 0 {
		t.Errorf("got %d aircraft, want 0", len(acs))
	}
}

func TestFetch_NonOKStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv.Close()
	_, err := NewADSBLol(srv.Client(), srv.URL, "airdash/test").Fetch(t.Context(), 0, 0, 10)
	if err == nil {
		t.Fatal("expected error on 429")
	}
}

func TestFetch_429IsErrRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := NewADSBLol(srv.Client(), srv.URL, "airdash/test").Fetch(t.Context(), 0, 0, 10)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestNew(t *testing.T) {
	if _, err := New("adsblol", http.DefaultClient, "ua"); err != nil {
		t.Error(err)
	}
	if _, err := New("adsbfi", http.DefaultClient, "ua"); err != nil {
		t.Error(err)
	}
	if _, err := New("nope", http.DefaultClient, "ua"); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestCorrectQNH(t *testing.T) {
	for _, c := range []struct {
		alt  int
		qnh  float64
		want int
	}{
		{1125, 1020.0, 1307}, // 1125 + 6.75*27 = 1307.25
		{-125, 1020.0, 57},   // -125 + 182.25
		{100, 990.0, 0},      // 100 - 627.75 clamped
	} {
		if got := correctQNH(c.alt, c.qnh); got != c.want {
			t.Errorf("correctQNH(%d, %v) = %d, want %d", c.alt, c.qnh, got, c.want)
		}
	}
}

func TestDecodeReadsb_QNH(t *testing.T) {
	body, err := os.ReadFile("testdata/qnh_mix.json")
	if err != nil {
		t.Fatal(err)
	}
	acs, err := decodeReadsb(body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"aaaaaa": 1307, // own QNH 1020
		"bbbbbb": 2182, // only A is a low sample, median 1020 -> 2000 + 6.75*27 = 2182.25
		"cccccc": 7000, // above transition altitude, and not a median sample
		"dddddd": 3182, // invalid own QNH -> median 1020
	}
	for _, a := range acs {
		if a.AltFt != want[a.Hex] {
			t.Errorf("%s AltFt = %d, want %d", a.Hex, a.AltFt, want[a.Hex])
		}
	}
}

// Cruising aircraft select standard 1013, so their nav_qnh says nothing
// about the local pressure and must not be borrowed by low aircraft.
func TestDecodeReadsb_QNHIgnoresHighAircraftForMedian(t *testing.T) {
	acs, err := decodeReadsb([]byte(`{"ac":[
{"hex":"a","alt_baro":2000},
{"hex":"b","alt_baro":35000,"nav_qnh":1013.6}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if acs[0].AltFt != 2000 {
		t.Errorf("low AltFt = %d, want 2000 (uncorrected)", acs[0].AltFt)
	}
}

func TestDecodeReadsb_NoQNHAnywhere(t *testing.T) {
	acs, err := decodeReadsb([]byte(`{"ac":[{"hex":"a","alt_baro":1500},{"hex":"b","alt_baro":2500}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if acs[0].AltFt != 1500 || acs[1].AltFt != 2500 {
		t.Errorf("altitudes changed: %d, %d", acs[0].AltFt, acs[1].AltFt)
	}
}

func TestDecodeReadsb_QNHSkipsUnknownAndGround(t *testing.T) {
	acs, err := decodeReadsb([]byte(`{"ac":[
{"hex":"a","alt_baro":2000,"nav_qnh":1020},
{"hex":"b","nav_qnh":1020},
{"hex":"c","alt_baro":"ground","nav_qnh":1020}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if acs[0].AltFt != 2182 { // 2000 + 6.75*27 = 2182.25
		t.Errorf("airborne AltFt = %d, want 2182", acs[0].AltFt)
	}
	if acs[1].AltFt != 0 {
		t.Errorf("no alt_baro AltFt = %d, want 0", acs[1].AltFt)
	}
	if !acs[2].OnGround || acs[2].AltFt != 0 {
		t.Errorf("ground: OnGround=%v AltFt=%d, want true/0", acs[2].OnGround, acs[2].AltFt)
	}
}

func TestDecodeReadsb_Category(t *testing.T) {
	acs, err := decodeReadsb([]byte(`{"ac":[{"hex":"a1","t":"C172","category":"A1"},{"hex":"a2","t":"A320","category":"A3"},{"hex":"a3"}]}`))
	if err != nil || len(acs) != 3 {
		t.Fatalf("err=%v n=%d", err, len(acs))
	}
	if acs[0].Category != "A1" || acs[1].Category != "A3" || acs[2].Category != "" {
		t.Errorf("categories = %q %q %q", acs[0].Category, acs[1].Category, acs[2].Category)
	}
}

func TestDecodeReadsbErrorBodyIsErrBadBody(t *testing.T) {
	_, err := decodeReadsb([]byte(`{"error":"quota"}`))
	if !errors.Is(err, ErrBadBody) || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v", err)
	}
	if _, err := decodeReadsb([]byte(`not json`)); !errors.Is(err, ErrBadBody) {
		t.Fatalf("decode failure err = %v", err)
	}
}
