// Command airdash polls a community ADS-B feed and serves the nearest
// aircraft to a fixed point for the T-Display firmware to render.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"airdash/internal/api"
	"airdash/internal/board"
	"airdash/internal/config"
	"airdash/internal/flightstatus"
	"airdash/internal/geocode"
	"airdash/internal/logos"
	"airdash/internal/provider"
	"airdash/internal/routes"
	"airdash/internal/settings"
	"airdash/internal/tracker"
)

const version = "0.1.0"
const userAgent = "airdash/" + version + " (+https://github.com/dbr486/airdash)"

// routeErrorCooldown bounds how often a failing adsbdb lookup is retried
// for the same callsign, so an outage does not turn into a retry storm
// against a free, unauthenticated API.
const routeErrorCooldown = 60 * time.Second

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the running service's /healthz and exit")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(os.Getenv("AIRDASH_LISTEN")))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "airdash:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	prov, err := provider.New(cfg.Provider, client, userAgent)
	if err != nil {
		return err
	}
	logoSrc := logos.New(client, logos.DefaultBaseURL, userAgent, time.Now)
	lookup := routes.NewCache(
		routes.NewADSBDB(client, routes.DefaultADSBDBURL, userAgent),
		cfg.RouteCacheTTL, time.Hour, routeErrorCooldown, time.Now)

	initial := settings.FromConfig(cfg)
	source := "env"
	if cfg.SettingsFile != "" {
		if s, found, err := settings.Load(cfg.SettingsFile, initial); err != nil {
			logger.Warn("settings file ignored", "path", cfg.SettingsFile, "err", err)
		} else if found {
			initial, source = s, "file"
		}
	}
	cur, err := settings.NewCurrent(initial, cfg.SettingsFile, logger)
	if err != nil {
		return err
	}
	logger.Info("settings loaded", "source", source, "origin", initial.Origin, "dest", initial.Dest,
		"airport", initial.Airport, "radius_km", initial.RadiusKm, "file", cfg.SettingsFile)
	// Delay lookups are opt-in on the presence of an AeroAPI key: without
	// one flightstatus.Lookup stays nil and the tracker skips the lookup
	// entirely (see tracker.Tick), so the feature costs nothing when unset.
	var delay flightstatus.Lookup
	var budget *flightstatus.Budget
	if cfg.AeroAPIKey != "" {
		budget = flightstatus.NewBudget(func() int { return cur.Settings().DelayLookupsPerDay }, time.Now, logger)
		delay = flightstatus.NewCache(
			flightstatus.NewBudgeted(flightstatus.NewAeroAPI(client, cfg.AeroAPIURL, cfg.AeroAPIKey, userAgent), budget),
			15*time.Minute, 15*time.Minute, time.Minute, time.Now)
		logger.Info("delay lookups enabled", "source", "aeroapi", "cap", cur.Settings().DelayLookupsPerDay)
	} else {
		logger.Info("delay lookups disabled (no AIRDASH_AEROAPI_KEY)")
	}
	// The airport board is a free delay source for every aircraft touching
	// the configured airport; it goes in front of AeroAPI so a board hit
	// never spends a paid lookup.
	var brd *board.Board
	if cfg.AirLabsKey != "" {
		brd = board.New(board.NewAirLabs(client, board.DefaultAirLabsURL, cfg.AirLabsKey, userAgent), cfg.BoardCallsPerMonth, 2*cfg.BoardRefresh, time.Now, logger)
		if cfg.SettingsFile != "" {
			// The month's call count lives next to the settings so a redeploy
			// cannot reset it and overrun the key's quota.
			if err := brd.Persist(filepath.Join(filepath.Dir(cfg.SettingsFile), "board-calls.json")); err != nil {
				logger.Warn("board call counter not loaded", "err", err)
			}
		}
		delay = board.Chain(brd, delay)
		logger.Info("airport board enabled", "airport", cur.Settings().BoardAirport, "refresh", cfg.BoardRefresh, "cap", cfg.BoardCallsPerMonth)
	}

	// Filter and RadiusKm come from cur (the Settings source) once Settings
	// is set, so cfg's own values here are only the pre-Settings fallback
	// Tick would use if Settings were ever nil; validation of whichever
	// initial settings actually win (env defaults or the file) already
	// happened in settings.NewCurrent above.
	opts := tracker.Options{
		Lat: cfg.Lat, Lon: cfg.Lon, RadiusKm: cfg.RadiusKm, Poll: cfg.Poll,
		Provider: prov, Routes: lookup, Settings: cur, Logger: logger, Delay: delay,
	}
	if brd != nil { // a typed nil in the interface would not be nil
		opts.BoardDelay = brd
	}
	tr := tracker.New(opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go tr.Run(ctx)
	if brd != nil {
		go brd.Run(ctx, func() string { return cur.Settings().BoardAirport }, cfg.BoardRefresh)
	}

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: api.NewHandler(tr, cur, delayStats{cur: cur, budget: budget, board: brd}, logoSrc, time.Now,
			api.WithGeocoder(geocode.New(client, geocode.DefaultURL, userAgent+" +https://github.com/dbr486/airdash", time.Now))),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	curFilter, radiusKm := cur.Get()
	logger.Info("airdash started", "version", version, "listen", cfg.Listen,
		"lat", cfg.Lat, "lon", cfg.Lon, "radius_km", radiusKm,
		"provider", cfg.Provider, "filter_active", curFilter.Active())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// delayStats answers /api/stats from the live settings and the budget.
type delayStats struct {
	cur    *settings.Current
	budget *flightstatus.Budget
	board  *board.Board
}

func (d delayStats) DelayEnabled() bool {
	return (d.budget != nil || d.board != nil) && d.cur.DelayWanted()
}
func (d delayStats) DelayToday() (int, int) {
	if d.budget == nil {
		return 0, d.cur.Settings().DelayLookupsPerDay
	}
	return d.budget.Today()
}
func (d delayStats) Board() (string, int, int, int, int, string) {
	if d.board == nil {
		return "", 0, -1, 0, 0, ""
	}
	s := d.board.Stats()
	return s.Airport, s.Flights, s.AgeS, s.CallsMonth, s.CallsCap, "airlabs"
}

func (d delayStats) DelaySource() string {
	switch {
	case d.board != nil && d.budget != nil:
		return "airlabs+aeroapi"
	case d.board != nil:
		return "airlabs"
	case d.budget != nil:
		return "aeroapi"
	}
	return ""
}

// runHealthcheck is what Docker calls; scratch images have no curl.
func runHealthcheck(listen string) int {
	if listen == "" {
		listen = ":8080"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
