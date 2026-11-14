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
	"airdash/internal/capture"
	"airdash/internal/config"
	"airdash/internal/flightstatus"
	"airdash/internal/geocode"
	"airdash/internal/logos"
	"airdash/internal/operators"
	"airdash/internal/provider"
	"airdash/internal/routes"
	"airdash/internal/settings"
	"airdash/internal/sourcelog"
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
	// The source log is a diagnostic: a file that cannot be opened turns it
	// off with a warning rather than keeping the display from starting. It
	// opens before the clients because they capture into it.
	var srcLog *sourcelog.Log
	// sink stays a nil interface unless the log opened; assigning a nil *Log
	// would make the capture wrapper's nil check pass and then panic.
	var sink capture.Sink
	if cfg.SourceLogFile != "" {
		if l, err := sourcelog.Open(cfg.SourceLogFile, time.Now, logger); err != nil {
			logger.Warn("route source log disabled", "file", cfg.SourceLogFile, "err", err)
		} else {
			srcLog = l
			sink = l
			logger.Info("route source log enabled", "file", cfg.SourceLogFile)
		}
	}
	apiOpts := []api.Option{api.WithGeocoder(geocode.New(client, geocode.DefaultURL, userAgent+" +https://github.com/dbr486/airdash", time.Now))}
	// Only the route and board lookups are captured: they are the answers the
	// source comparison can disagree about, and the raw body is what explains
	// a disagreement. Owner, logo, geocoder and position traffic is either
	// not part of that comparison or not JSON worth keeping.
	adsbdbClient := capture.Wrap(client, sink, tracker.SourceADSBDB, capture.SubjectLastSegment, time.Now)
	hexdbClient := capture.Wrap(client, sink, tracker.SourceHexDB, capture.SubjectHexDB, time.Now)
	aeroClient := capture.Wrap(client, sink, tracker.SourceAeroAPI, capture.SubjectLastSegment, time.Now)
	airlabsClient := capture.Wrap(client, sink, capture.SourceAirLabs, capture.SubjectAirLabs, time.Now)
	logoSrc := logos.New(client, logos.DefaultBaseURL, userAgent, time.Now)
	lookup := routes.NewCache(
		routes.NewADSBDB(adsbdbClient, routes.DefaultADSBDBURL, userAgent),
		cfg.RouteCacheTTL, time.Hour, routeErrorCooldown, time.Now)
	fallbackRoutes := routes.NewCache(
		routes.NewHexDB(hexdbClient, routes.DefaultHexDBURL, userAgent, time.Now),
		cfg.RouteCacheTTL, time.Hour, routeErrorCooldown, time.Now)

	// Owner answers change rarely, so known ones are cached for a week;
	// unknown ones for a day in case the registry gets filled in.
	opResolver := operators.NewResolver(operators.NewCache(
		operators.NewADSBDBOwner(client, routes.DefaultADSBDBURL, userAgent),
		7*24*time.Hour, 24*time.Hour, routeErrorCooldown, time.Now))

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
	// aero is kept apart from delay because the tracker's route fallback must
	// ask this very instance: one cache entry per ident then answers both the
	// route and the delay, so one aircraft never costs two paid calls.
	var aero *flightstatus.Cache
	if cfg.AeroAPIKey != "" {
		budget = flightstatus.NewBudget(func() int { return cur.Settings().DelayLookupsPerDay }, time.Now, logger)
		aero = flightstatus.NewCache(
			flightstatus.NewBudgeted(flightstatus.NewAeroAPI(aeroClient, cfg.AeroAPIURL, cfg.AeroAPIKey, userAgent), budget),
			15*time.Minute, 15*time.Minute, time.Minute, time.Now)
		delay = aero
		logger.Info("delay lookups enabled", "source", "aeroapi", "cap", cur.Settings().DelayLookupsPerDay)
	} else {
		logger.Info("delay lookups disabled (no AIRDASH_AEROAPI_KEY)")
	}
	// The airport board is a free delay source for every aircraft touching
	// the configured airport; it goes in front of AeroAPI so a board hit
	// never spends a paid lookup.
	var brd *board.Board
	if cfg.AirLabsKey != "" {
		brd = board.New(board.NewAirLabs(airlabsClient, board.DefaultAirLabsURL, cfg.AirLabsKey, userAgent), cfg.BoardCallsPerMonth, 2*cfg.BoardRefresh, time.Now, logger)
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
		Provider: prov, Routes: lookup, FallbackRoutes: fallbackRoutes, Operators: opResolver, Settings: cur, Logger: logger, Delay: delay,
	}
	if brd != nil { // a typed nil in the interface would not be nil
		opts.BoardDelay = brd
		opts.BoardRoute = brd
	}
	if aero != nil {
		opts.Flights = aero
	}
	if srcLog != nil {
		opts.Recorder = srcLog
		apiOpts = append(apiOpts, api.WithSourceLog(srcLog))
	}
	tr := tracker.New(opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	trDone := make(chan struct{})
	go func() { tr.Run(ctx); close(trDone) }()
	// The writer stops only after the tracker has: a tick still finishing
	// when ctx is cancelled may enqueue one last batch, and the writer drains
	// what is queued when its own context ends.
	logCtx, stopLog := context.WithCancel(context.Background())
	defer stopLog()
	logDone := make(chan struct{})
	if srcLog != nil {
		go func() { srcLog.Run(logCtx); close(logDone) }()
	} else {
		close(logDone)
	}
	if brd != nil {
		go brd.Run(ctx, func() string { return cur.Settings().BoardAirport }, cfg.BoardRefresh)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.NewHandler(tr, cur, delayStats{cur: cur, budget: budget, board: brd}, logoSrc, time.Now, apiOpts...),
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
	shutdownErr := srv.Shutdown(shutdownCtx)
	if errors.Is(shutdownErr, http.ErrServerClosed) {
		shutdownErr = nil
	}
	if shutdownErr != nil {
		_ = srv.Close() // a request outlived the grace period; drop it
	}
	// The source log is flushed whatever happened above: the tracker stops
	// first, then the writer drains what is queued, then the file closes.
	<-trDone
	stopLog()
	<-logDone
	if srcLog != nil {
		if err := srcLog.Close(); err != nil {
			logger.Warn("route source log close failed", "err", err)
		}
	}
	return shutdownErr
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

// PaidRoutesEnabled needs the budget (non-nil only with an AeroAPI key) so
// the page never claims paid routes work when there is no key to pay with.
func (d delayStats) PaidRoutesEnabled() bool {
	return d.budget != nil && d.cur.PaidRoutesWanted()
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
