// Package config reads the service configuration from environment
// variables. Everything is read once at startup; a bad value is fatal so
// that a misconfigured container fails loudly instead of showing nothing.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Lat, Lon      float64
	RadiusKm      float64
	Poll          time.Duration
	Listen        string
	Provider      string
	Origin        string
	Dest          string
	Airport       string
	SettingsFile  string
	RouteCacheTTL time.Duration
	AeroAPIKey    string
	AeroAPIURL    string
	// Airport board (AirLabs): key, airport, refresh interval and the
	// monthly call cap the service enforces on itself.
	AirLabsKey         string
	BoardAirport       string
	BoardRefresh       time.Duration
	BoardCallsPerMonth int
}

var providers = map[string]bool{"adsblol": true, "adsbfi": true}

func Load(getenv func(string) string) (Config, error) {
	var c Config
	var err error

	if c.Lat, err = requiredFloat(getenv, "AIRDASH_LAT", -90, 90); err != nil {
		return c, err
	}
	if c.Lon, err = requiredFloat(getenv, "AIRDASH_LON", -180, 180); err != nil {
		return c, err
	}
	if c.RadiusKm, err = optionalFloat(getenv, "AIRDASH_RADIUS_KM", 10); err != nil {
		return c, err
	}
	if c.RadiusKm <= 0 {
		return c, fmt.Errorf("AIRDASH_RADIUS_KM must be greater than 0")
	}
	// adsb.fi (the default provider below) allows 1 request/s, so a 5 s
	// poll is comfortable; adsb.lol rate-limits at roughly 8 requests per
	// minute per IP and needs AIRDASH_POLL_SECONDS of 10 or more.
	poll, err := optionalFloat(getenv, "AIRDASH_POLL_SECONDS", 5)
	if err != nil {
		return c, err
	}
	if poll < 2 {
		return c, fmt.Errorf("AIRDASH_POLL_SECONDS must be at least 2")
	}
	c.Poll = time.Duration(poll * float64(time.Second))

	c.Listen = getenv("AIRDASH_LISTEN")
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	// adsb.fi tolerates a much higher poll rate than adsb.lol (see the
	// AIRDASH_POLL_SECONDS comment above), so it is the safer default.
	c.Provider = getenv("AIRDASH_PROVIDER")
	if c.Provider == "" {
		c.Provider = "adsbfi"
	}
	if !providers[c.Provider] {
		return c, fmt.Errorf("AIRDASH_PROVIDER %q is not one of adsblol, adsbfi", c.Provider)
	}
	c.Origin = getenv("AIRDASH_ORIGIN")
	c.Dest = getenv("AIRDASH_DEST")
	c.Airport = getenv("AIRDASH_AIRPORT")
	c.SettingsFile = getenv("AIRDASH_SETTINGS_FILE")

	c.AeroAPIKey = getenv("AIRDASH_AEROAPI_KEY")
	c.AeroAPIURL = getenv("AIRDASH_AEROAPI_URL")
	if c.AeroAPIURL == "" {
		// Mirrors flightstatus.DefaultAeroAPIURL's value; config must not
		// import the flightstatus package, so the literal is repeated here.
		c.AeroAPIURL = "https://aeroapi.flightaware.com/aeroapi"
	}

	c.AirLabsKey = getenv("AIRDASH_AIRLABS_KEY")
	c.BoardAirport = strings.ToUpper(strings.TrimSpace(getenv("AIRDASH_BOARD_AIRPORT")))
	if c.BoardAirport != "" && (len(c.BoardAirport) < 3 || len(c.BoardAirport) > 4) {
		return c, fmt.Errorf("AIRDASH_BOARD_AIRPORT must be a 3-letter IATA or 4-letter ICAO code")
	}
	c.BoardRefresh = 100 * time.Minute // 2 calls per refresh ≈ 890/month, under the 900 cap
	if v := getenv("AIRDASH_BOARD_REFRESH_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 {
			return c, fmt.Errorf("AIRDASH_BOARD_REFRESH_MINUTES must be an integer of at least 5")
		}
		c.BoardRefresh = time.Duration(n) * time.Minute
	}
	c.BoardCallsPerMonth = 900
	if v := getenv("AIRDASH_BOARD_CALLS_PER_MONTH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("AIRDASH_BOARD_CALLS_PER_MONTH must be a positive integer")
		}
		c.BoardCallsPerMonth = n
	}

	hours, err := optionalFloat(getenv, "AIRDASH_ROUTE_CACHE_HOURS", 6)
	if err != nil {
		return c, err
	}
	if hours <= 0 {
		return c, fmt.Errorf("AIRDASH_ROUTE_CACHE_HOURS must be greater than 0")
	}
	c.RouteCacheTTL = time.Duration(hours * float64(time.Hour))
	return c, nil
}

func requiredFloat(getenv func(string) string, key string, lo, hi float64) (float64, error) {
	raw := getenv(key)
	if raw == "" {
		return 0, fmt.Errorf("%s is required", key)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", key, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s: %q is not a finite number", key, raw)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%s must be between %v and %v", key, lo, hi)
	}
	return v, nil
}

func optionalFloat(getenv func(string) string, key string, def float64) (float64, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", key, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s: %q is not a finite number", key, raw)
	}
	return v, nil
}
