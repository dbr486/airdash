package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAirLabs_Departures(t *testing.T) {
	rows, err := ParseAirLabs(fixture(t, "airlabs_dep_agp.json"))
	if err != nil || len(rows) != 12 {
		t.Fatalf("len=%d err=%v", len(rows), err)
	}
	r := rows[1] // WMT6026 AGP→FCO, actual 13:24, +9
	if r.FlightICAO != "WMT6026" || r.FlightIATA != "W46026" || r.AirlineIATA != "W4" || r.DepIATA != "AGP" || r.ArrIATA != "FCO" || r.Status != "active" {
		t.Errorf("row = %+v", r)
	}
	want := time.Date(2026, 9, 29, 13, 24, 0, 0, time.UTC)
	if !r.DepActual.Equal(want) || !r.DepSched.Equal(want.Add(-9*time.Minute)) || r.DepDelayMin == nil || *r.DepDelayMin != 9 || r.ArrDelayMin != nil || !r.ArrEst.IsZero() {
		t.Errorf("times/delays = %+v", r)
	}
	if rows[2].DepDelayMin != nil { // RYR4687 left early: the board sends null
		t.Errorf("null delay must stay nil: %+v", rows[2])
	}
}

func TestParseAirLabs_ArrivalsNullAirlineAndCancelled(t *testing.T) {
	rows, err := ParseAirLabs(fixture(t, "airlabs_arr_agp.json"))
	if err != nil || len(rows) != 12 {
		t.Fatalf("len=%d err=%v", len(rows), err)
	}
	if rows[0].FlightICAO != "HTY251" || rows[0].AirlineIATA != "" || rows[0].FlightIATA != "" || rows[0].ArrDelayMin == nil || *rows[0].ArrDelayMin != 9 {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].Status != "cancelled" || rows[1].FlightICAO != "IBE5834" {
		t.Errorf("row1 = %+v", rows[1])
	}
}

func TestParseAirLabs_ErrorEnvelopeAndGarbage(t *testing.T) {
	if _, err := ParseAirLabs([]byte(`{"error":{"message":"Unknown api_key","code":"unknown_api_key"}}`)); err == nil || !strings.Contains(err.Error(), "unknown_api_key") {
		t.Errorf("error envelope must surface: %v", err)
	}
	if _, err := ParseAirLabs([]byte(`nope`)); err == nil {
		t.Error("garbage must error")
	}
	if rows, err := ParseAirLabs([]byte(`{"response":[]}`)); err != nil || len(rows) != 0 {
		t.Errorf("empty board: %v %v", rows, err)
	}
}

func TestAirLabs_FetchBuildsTheQuery(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "airlabs_arr_agp.json"))
	}))
	defer srv.Close()
	a := NewAirLabs(srv.Client(), srv.URL, "secret", "airdash/test")
	rows, err := a.Fetch(context.Background(), "AGP", true)
	if err != nil || len(rows) != 12 {
		t.Fatalf("len=%d err=%v", len(rows), err)
	}
	if !strings.HasPrefix(got, "/schedules?") || !strings.Contains(got, "arr_iata=AGP") || !strings.Contains(got, "limit=500") || !strings.Contains(got, "api_key=secret") {
		t.Errorf("url = %s", got)
	}
	if _, err := a.Fetch(context.Background(), "AGP", false); err != nil || !strings.Contains(got, "dep_iata=AGP") {
		t.Errorf("departures url = %s err=%v", got, err)
	}
}

func TestAirLabs_FetchICAOAirportAndHTTPError(t *testing.T) {
	var got string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"response":[]}`))
	}))
	defer srv.Close()
	a := NewAirLabs(srv.Client(), srv.URL, "k", "ua")
	if _, err := a.Fetch(context.Background(), "LEMG", false); err != nil || !strings.Contains(got, "dep_icao=LEMG") {
		t.Errorf("icao: %s err=%v", got, err)
	}
	status = 500
	if _, err := a.Fetch(context.Background(), "AGP", false); err == nil {
		t.Error("500 must error")
	}
}

func TestAirLabs_ErrorNeverContainsTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := srv.URL
	srv.Close() // connection refused from now on
	a := NewAirLabs(&http.Client{Timeout: time.Second}, u, "s3cr3tkey", "ua")
	_, err := a.Fetch(context.Background(), "AGP", false)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "s3cr3tkey") || strings.Contains(err.Error(), "api_key") {
		t.Errorf("the key must never reach an error message: %v", err)
	}
}
