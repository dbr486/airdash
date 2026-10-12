package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		"AIRDASH_LAT": "51.47",
		"AIRDASH_LON": "-0.45",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Lat != 51.47 || c.Lon != -0.45 {
		t.Errorf("coords = %v,%v", c.Lat, c.Lon)
	}
	if c.RadiusKm != 10 {
		t.Errorf("RadiusKm = %v, want 10", c.RadiusKm)
	}
	if c.Poll != 5*time.Second {
		t.Errorf("Poll = %v, want 5s", c.Poll)
	}
	if c.Listen != ":8080" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.Provider != "adsbfi" {
		t.Errorf("Provider = %q", c.Provider)
	}
	if c.RouteCacheTTL != 6*time.Hour {
		t.Errorf("RouteCacheTTL = %v", c.RouteCacheTTL)
	}
	if c.Origin != "" || c.Dest != "" || c.Airport != "" {
		t.Errorf("filters should be empty by default")
	}
	if c.SettingsFile != "" {
		t.Errorf("SettingsFile = %q", c.SettingsFile)
	}
	if c.AeroAPIKey != "" {
		t.Errorf("AeroAPIKey = %q", c.AeroAPIKey)
	}
	if c.AeroAPIURL != "https://aeroapi.flightaware.com/aeroapi" {
		t.Errorf("AeroAPIURL = %q", c.AeroAPIURL)
	}
}

func TestLoad_Overrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"AIRDASH_LAT":               "1",
		"AIRDASH_LON":               "2",
		"AIRDASH_RADIUS_KM":         "25.5",
		"AIRDASH_POLL_SECONDS":      "7",
		"AIRDASH_LISTEN":            "127.0.0.1:9000",
		"AIRDASH_PROVIDER":          "adsblol",
		"AIRDASH_ORIGIN":            "LHR,LGW",
		"AIRDASH_DEST":              "BCN",
		"AIRDASH_AIRPORT":           "EGLL",
		"AIRDASH_SETTINGS_FILE":     "/data/settings.json",
		"AIRDASH_ROUTE_CACHE_HOURS": "2",
		"AIRDASH_AEROAPI_KEY":       "k123",
		"AIRDASH_AEROAPI_URL":       "http://127.0.0.1:1",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.RadiusKm != 25.5 || c.Poll != 7*time.Second || c.Listen != "127.0.0.1:9000" ||
		c.Provider != "adsblol" || c.Origin != "LHR,LGW" || c.Dest != "BCN" ||
		c.Airport != "EGLL" || c.SettingsFile != "/data/settings.json" || c.RouteCacheTTL != 2*time.Hour {
		t.Errorf("unexpected config: %+v", c)
	}
	if c.AeroAPIKey != "k123" || c.AeroAPIURL != "http://127.0.0.1:1" {
		t.Errorf("aeroapi = %q %q", c.AeroAPIKey, c.AeroAPIURL)
	}
}

func TestLoad_Rejects(t *testing.T) {
	base := map[string]string{"AIRDASH_LAT": "51.47", "AIRDASH_LON": "-0.45"}
	cases := []struct {
		name, key, val, wantIn string
	}{
		{"missing lat", "AIRDASH_LAT", "", "AIRDASH_LAT"},
		{"missing lon", "AIRDASH_LON", "", "AIRDASH_LON"},
		{"lat out of range", "AIRDASH_LAT", "91", "AIRDASH_LAT"},
		{"lon out of range", "AIRDASH_LON", "-181", "AIRDASH_LON"},
		{"lat not a number", "AIRDASH_LAT", "abc", "AIRDASH_LAT"},
		{"lat NaN", "AIRDASH_LAT", "NaN", "AIRDASH_LAT"},
		{"lon Inf", "AIRDASH_LON", "Inf", "AIRDASH_LON"},
		{"radius zero", "AIRDASH_RADIUS_KM", "0", "AIRDASH_RADIUS_KM"},
		{"radius negative", "AIRDASH_RADIUS_KM", "-3", "AIRDASH_RADIUS_KM"},
		{"radius NaN", "AIRDASH_RADIUS_KM", "NaN", "AIRDASH_RADIUS_KM"},
		{"radius Inf", "AIRDASH_RADIUS_KM", "Inf", "AIRDASH_RADIUS_KM"},
		{"poll too small", "AIRDASH_POLL_SECONDS", "1", "AIRDASH_POLL_SECONDS"},
		{"poll not a number", "AIRDASH_POLL_SECONDS", "fast", "AIRDASH_POLL_SECONDS"},
		{"poll NaN", "AIRDASH_POLL_SECONDS", "NaN", "AIRDASH_POLL_SECONDS"},
		{"unknown provider", "AIRDASH_PROVIDER", "flightradar", "AIRDASH_PROVIDER"},
		{"cache hours zero", "AIRDASH_ROUTE_CACHE_HOURS", "0", "AIRDASH_ROUTE_CACHE_HOURS"},
		{"cache hours +Inf", "AIRDASH_ROUTE_CACHE_HOURS", "+Inf", "AIRDASH_ROUTE_CACHE_HOURS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range base {
				m[k] = v
			}
			m[tc.key] = tc.val
			_, err := Load(env(m))
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name %s", err, tc.wantIn)
			}
		})
	}
}

func TestLoad_BoardDefaultsAndOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{"AIRDASH_LAT": "1", "AIRDASH_LON": "2"}))
	if err != nil || c.BoardAirport != "" || c.BoardRefresh != 100*time.Minute || c.BoardCallsPerMonth != 900 || c.AirLabsKey != "" {
		t.Errorf("defaults: %+v err=%v", c, err)
	}
	c, err = Load(env(map[string]string{"AIRDASH_LAT": "1", "AIRDASH_LON": "2", "AIRDASH_AIRLABS_KEY": "k",
		"AIRDASH_BOARD_AIRPORT": "agp", "AIRDASH_BOARD_REFRESH_MINUTES": "30", "AIRDASH_BOARD_CALLS_PER_MONTH": "5000"}))
	if err != nil || c.BoardAirport != "AGP" || c.BoardRefresh != 30*time.Minute || c.BoardCallsPerMonth != 5000 || c.AirLabsKey != "k" {
		t.Errorf("overrides: %+v err=%v", c, err)
	}
	for _, bad := range []map[string]string{{"AIRDASH_BOARD_REFRESH_MINUTES": "4"}, {"AIRDASH_BOARD_CALLS_PER_MONTH": "0"}, {"AIRDASH_BOARD_AIRPORT": "A"}} {
		m := map[string]string{"AIRDASH_LAT": "1", "AIRDASH_LON": "2"}
		for k, v := range bad {
			m[k] = v
		}
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
}
