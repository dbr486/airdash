package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
