// Package settings holds the user-changeable part of the configuration
// (airport filters and radius) so the web UI can change it at runtime and
// the T-Display follows on its next poll. Env vars are the defaults; a
// JSON file, when configured, overrides them and receives every change.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"airdash/internal/config"
	"airdash/internal/filter"
)

// ErrValidation marks an error returned by Normalize (as opposed to an I/O
// failure elsewhere in the package), so callers can classify it with
// errors.Is instead of matching on error text.
var ErrValidation = errors.New("invalid settings")

type Settings struct {
	// Home position the display watches from; defaults come from the
	// environment and the page can move them.
	Lat                float64 `json:"lat"`
	Lon                float64 `json:"lon"`
	Origin             string  `json:"origin"`
	Dest               string  `json:"dest"`
	Airport            string  `json:"airport"`
	RadiusKm           float64 `json:"radius_km"`
	ShowDelay          bool    `json:"show_delay"`
	DelayLookupsPerDay int     `json:"delay_lookups_per_day"`
	// ShowNames renders city names instead of airport codes on the route
	// row of both clients.
	ShowNames bool `json:"show_names"`
	// ShowLogos shows the airline logo beside the callsign (web mirror now,
	// display later). Presentation only: /api/logo is served regardless.
	ShowLogos bool `json:"show_logos"`
	// BoardAirport is the airport whose departures/arrivals board supplies
	// delay status; "" = off.
	BoardAirport string `json:"board_airport"`
	// PaidRoutes lets an AeroAPI lookup fill in a route the free sources
	// could not find. It spends the same daily cap as delay lookups, so it
	// has its own switch independent of ShowDelay.
	PaidRoutes bool `json:"paid_routes"`
	// Service caps, read live so a change applies on the next call.
	// AirLabsCallsPerMonth also sets the board's refresh interval (spread
	// over the month); the free-source caps are per UTC day, 0 = no cap.
	AirLabsCallsPerMonth int `json:"airlabs_calls_per_month"`
	ADSBDBCallsPerDay    int `json:"adsbdb_calls_per_day"`
	HexDBCallsPerDay     int `json:"hexdb_calls_per_day"`
}

// maxCallsCap bounds every service cap: far beyond any plan in use, it
// only stops a typo from meaning "unlimited".
const maxCallsCap = 100000

// DefaultDelayLookupsPerDay caps AeroAPI calls so the worst case (the cap
// hit every day) stays near $10 a month: 65/day x 31 days x $0.005 ≈
// $10.08, about $5 beyond the Personal tier's $5 monthly credit, a spend
// the owner accepted for paid routes. The cap is shared by delay and paid
// route lookups.
const DefaultDelayLookupsPerDay = 65

func FromConfig(c config.Config) Settings {
	return Settings{Lat: c.Lat, Lon: c.Lon, Origin: c.Origin, Dest: c.Dest, Airport: c.Airport, RadiusKm: c.RadiusKm,
		ShowDelay: c.AeroAPIKey != "" || c.AirLabsKey != "", DelayLookupsPerDay: DefaultDelayLookupsPerDay, ShowLogos: true, BoardAirport: c.BoardAirport,
		// Paid routes follow the key alone: a route-only call is useless without
		// one, and a persisted file without this key must inherit this default.
		PaidRoutes: c.AeroAPIKey != "",
		// The env value only seeds the monthly cap; Settings owns it after.
		AirLabsCallsPerMonth: c.BoardCallsPerMonth}
}

// Normalize upper-cases and trims the code lists, validates them through
// the filter parser, and checks the radius. It returns the normalized
// settings and the compiled filter.
func Normalize(s Settings) (Settings, filter.Filter, error) {
	if math.IsNaN(s.Lat) || math.IsInf(s.Lat, 0) || s.Lat < -90 || s.Lat > 90 {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: lat must be between -90 and 90", ErrValidation)
	}
	if math.IsNaN(s.Lon) || math.IsInf(s.Lon, 0) || s.Lon < -180 || s.Lon > 180 {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: lon must be between -180 and 180", ErrValidation)
	}
	if math.IsNaN(s.RadiusKm) || math.IsInf(s.RadiusKm, 0) || s.RadiusKm <= 0 {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: radius_km must be a finite number greater than 0", ErrValidation)
	}
	if s.RadiusKm > 250 {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: radius_km must be at most 250", ErrValidation)
	}
	if s.DelayLookupsPerDay < 0 || s.DelayLookupsPerDay > 1000 {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: delay_lookups_per_day must be between 0 and 1000", ErrValidation)
	}
	// 0 stops board refreshes, as delay_lookups_per_day 0 stops AeroAPI.
	if s.AirLabsCallsPerMonth < 0 || s.AirLabsCallsPerMonth > maxCallsCap {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: airlabs_calls_per_month must be between 0 and %d", ErrValidation, maxCallsCap)
	}
	if s.ADSBDBCallsPerDay < 0 || s.ADSBDBCallsPerDay > maxCallsCap {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: adsbdb_calls_per_day must be between 0 (no cap) and %d", ErrValidation, maxCallsCap)
	}
	if s.HexDBCallsPerDay < 0 || s.HexDBCallsPerDay > maxCallsCap {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: hexdb_calls_per_day must be between 0 (no cap) and %d", ErrValidation, maxCallsCap)
	}
	ba := strings.ToUpper(strings.TrimSpace(s.BoardAirport))
	if ba != "" && (len(ba) < 3 || len(ba) > 4 || !alnum(ba)) {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: board_airport must be a 3-letter IATA or 4-letter ICAO code", ErrValidation)
	}
	n := Settings{
		Lat:                s.Lat,
		Lon:                s.Lon,
		BoardAirport:       ba,
		Origin:             normalizeList(s.Origin),
		Dest:               normalizeList(s.Dest),
		Airport:            normalizeList(s.Airport),
		RadiusKm:           s.RadiusKm,
		ShowDelay:          s.ShowDelay,
		DelayLookupsPerDay: s.DelayLookupsPerDay,
		ShowNames:          s.ShowNames,
		ShowLogos:          s.ShowLogos,
		PaidRoutes:         s.PaidRoutes,

		AirLabsCallsPerMonth: s.AirLabsCallsPerMonth,
		ADSBDBCallsPerDay:    s.ADSBDBCallsPerDay,
		HexDBCallsPerDay:     s.HexDBCallsPerDay,
	}
	f, err := filter.Parse(n.Origin, n.Dest, n.Airport)
	if err != nil {
		return Settings{}, filter.Filter{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return n, f, nil
}

func alnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func normalizeList(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		parts[i] = strings.ToUpper(strings.TrimSpace(p))
	}
	return strings.Join(parts, ",")
}

// Load reads path and decodes its JSON onto a copy of defaults, so any key
// missing from the file (e.g. one written before a new Settings field
// existed) keeps that field's default instead of silently zeroing it.
func Load(path string, defaults Settings) (Settings, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	s := defaults
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, false, fmt.Errorf("settings file: %w", err)
	}
	n, _, err := Normalize(s)
	if err != nil {
		return Settings{}, false, fmt.Errorf("settings file: %w", err)
	}
	return n, true, nil
}

// Save writes atomically so a crash mid-write never leaves a half file.
func Save(path string, s Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush to disk before the rename so a crash right after a successful
	// rename can never expose a file whose content didn't actually survive
	// the write (the rename itself only reorders the directory entry, not
	// the data behind it).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

type Current struct {
	path   string
	logger *slog.Logger

	// writeMu serializes the whole Normalize->Save->swap sequence in Set so
	// two concurrent PUTs can't interleave their file write and in-memory
	// swap (each still valid alone, but the last write to the file might
	// not be the last swap into memory). mu still guards readers against
	// the swap itself.
	writeMu sync.Mutex

	mu       sync.RWMutex
	settings Settings
	filter   filter.Filter
}

func NewCurrent(initial Settings, path string, logger *slog.Logger) (*Current, error) {
	n, f, err := Normalize(initial)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Current{path: path, logger: logger, settings: n, filter: f}, nil
}

func (c *Current) Settings() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings
}

func (c *Current) Get() (filter.Filter, float64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.filter, c.settings.RadiusKm
}

// Set validates first and persists before swapping, so an invalid request
// or a failed write leaves the running settings untouched.
func (c *Current) Set(s Settings) (Settings, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.setLocked(s)
}

// Update applies a read-modify-write against the current settings as a
// single atomic step: the read, apply, Normalize, Save and swap all happen
// under writeMu, so a partial merge (e.g. the API's PUT patch, which only
// changes the fields present in the request body) can't race with another
// Set/Update in between the read and the write and silently lose that
// other caller's change.
func (c *Current) Update(apply func(Settings) Settings) (Settings, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.RLock()
	cur := c.settings
	c.mu.RUnlock()
	return c.setLocked(apply(cur))
}

// setLocked runs the shared Normalize->Save->swap sequence; callers must
// hold writeMu.
func (c *Current) setLocked(s Settings) (Settings, error) {
	n, f, err := Normalize(s)
	if err != nil {
		return Settings{}, err
	}
	if c.path != "" {
		if err := Save(c.path, n); err != nil {
			return Settings{}, fmt.Errorf("save settings: %w", err)
		}
	}
	c.mu.Lock()
	c.settings, c.filter = n, f
	c.mu.Unlock()
	c.logger.Info("settings changed", "origin", n.Origin, "dest", n.Dest, "airport", n.Airport, "radius_km", n.RadiusKm,
		"show_delay", n.ShowDelay, "delay_cap", n.DelayLookupsPerDay, "paid_routes", n.PaidRoutes)
	return n, nil
}

// DelayWanted reports whether delay lookups should run at all: the user
// switched them on and the daily cap is not zero. Whether a key exists is
// main's concern (it passes a nil lookup when there is none).
func (c *Current) DelayWanted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings.ShowDelay && c.settings.DelayLookupsPerDay > 0
}

// PaidRoutesWanted reports whether a route-only AeroAPI call may run:
// switched on and the shared daily cap not zero (cap 0 turns every paid
// lookup off). The key check is main's concern, as for DelayWanted.
func (c *Current) PaidRoutesWanted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings.PaidRoutes && c.settings.DelayLookupsPerDay > 0
}

// ShowDelay is the user's wish alone, without the paid cap: the airport
// board costs nothing per aircraft, so it follows this and not DelayWanted.
func (c *Current) ShowDelay() bool { return c.Settings().ShowDelay }

// Home is the position to watch from; the tracker asks each tick so a
// move on the page applies without a restart.
func (c *Current) Home() (lat, lon float64, ok bool) {
	s := c.Settings()
	return s.Lat, s.Lon, true
}
