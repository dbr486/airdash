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
	"airdash/internal/usage"
)

const version = "0.1.0"
const userAgent = "airdash/" + version + " (+https://github.com/dbr486/airdash)"

// routeErrorCooldown bounds how often a failing route lookup is retried
// for the same callsign, so an outage does not turn into a retry storm
// against a free, unauthenticated API. A 429 from adsbdb is handled
// source-wide by routes.Gate instead; see there.
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
	// One meter per outside service: every network round trip is counted
	// at the transport, so cache hits and gate refusals never show as calls.
	aeroMeter, airlabsMeter := usage.NewMeter(time.Now), usage.NewMeter(time.Now)
	adsbdbMeter, hexdbMeter := usage.NewMeter(time.Now), usage.NewMeter(time.Now)
	posMeter := usage.NewMeter(time.Now)
	prov, err := provider.New(cfg.Provider, posMeter.Wrap(client, nil), userAgent)
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
	// One gate for every adsbdb client: its rate limit is per IP, so an
	// owner lookup during a ban would keep the route lookups banned too. It
	// sits outside the capture so a request it refuses leaves no row.
	// The meter sits inside the gate, so a request the gate refuses counts
	// as refused rather than as a call.
	adsbdbGate := routes.NewGate(time.Now, logger)
	adsbdbGate.OnRefuse(func(r *http.Request) { adsbdbMeter.Refuse(capture.SubjectLastSegment(r)) })
	// The daily caps are read from Settings, which load further down; the
	// closures only run once lookups start, after cur is set.
	var cur *settings.Current
	adsbdbCap := func() int { return cur.Settings().ADSBDBCallsPerDay }
	hexdbCap := func() int { return cur.Settings().HexDBCallsPerDay }
	// The cap limiter wraps everything else: a request it refuses never
	// reaches the gate, the meter (as a call) or upstream.
	adsbdbClient := adsbdbMeter.Limit(adsbdbGate.Wrap(adsbdbMeter.Wrap(capture.Wrap(client, sink, tracker.SourceADSBDB, capture.SubjectLastSegment, time.Now), capture.SubjectLastSegment)), adsbdbCap, capture.SubjectLastSegment)
	hexdbClient := hexdbMeter.Limit(hexdbMeter.Wrap(capture.Wrap(client, sink, tracker.SourceHexDB, capture.SubjectHexDB, time.Now), capture.SubjectHexDB), hexdbCap, capture.SubjectHexDB)
	aeroClient := aeroMeter.Wrap(capture.Wrap(client, sink, tracker.SourceAeroAPI, capture.SubjectLastSegment, time.Now), capture.SubjectLastSegment)
	airlabsClient := airlabsMeter.Wrap(capture.Wrap(client, sink, capture.SourceAirLabs, capture.SubjectAirLabs, time.Now), capture.SubjectAirLabs)
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
		operators.NewADSBDBOwner(adsbdbMeter.Limit(adsbdbGate.Wrap(adsbdbMeter.Wrap(client, capture.SubjectLastSegment)), adsbdbCap, capture.SubjectLastSegment), routes.DefaultADSBDBURL, userAgent),
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
	cur, err = settings.NewCurrent(initial, cfg.SettingsFile, logger)
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
		budget.OnRefuse(aeroMeter.Refuse)
		if cfg.SettingsFile != "" {
			// Like the board's month count, the day's count lives next to the
			// settings so a redeploy cannot reset it and overspend the cap.
			if err := budget.Persist(filepath.Join(filepath.Dir(cfg.SettingsFile), "aeroapi-budget.json")); err != nil {
				logger.Warn("delay budget counter not loaded", "err", err)
			}
		}
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
		// boardStaleFloor keeps rows usable for the old 2 x 100 min while the
		// cap is spent and no interval applies.
		const boardStaleFloor = 200 * time.Minute
		brd = board.New(board.NewAirLabs(airlabsClient, board.DefaultAirLabsURL, cfg.AirLabsKey, userAgent), cfg.BoardCallsPerMonth, boardStaleFloor, time.Now, logger)
		brd.SetCapFunc(func() int { return cur.Settings().AirLabsCallsPerMonth })
		if cfg.SettingsFile != "" {
			// The month's call count lives next to the settings so a redeploy
			// cannot reset it and overrun the key's quota.
			if err := brd.Persist(filepath.Join(filepath.Dir(cfg.SettingsFile), "board-calls.json")); err != nil {
				logger.Warn("board call counter not loaded", "err", err)
			}
		}
		brd.OnRefuse(airlabsMeter.Refuse)
		brd.OnFail(airlabsMeter.Fail)
		delay = board.Chain(brd, delay)
		iv, _ := brd.Schedule()
		logger.Info("airport board enabled", "airport", cur.Settings().BoardAirport, "refresh", iv, "cap", cur.Settings().AirLabsCallsPerMonth)
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
	opts.PositionFailure = func(msg string) { posMeter.Fail("", msg) }
	limits := usage.NewRegistry(time.Now, logger)
	aeroLimit := usage.Limit{Enabled: budget != nil, Period: "day"}
	if budget != nil {
		aeroLimit.Used = func() (int, int, time.Time) {
			used, limit := budget.Today()
			return used, limit, budget.CapHitAt()
		}
		aeroLimit.Blocked = budget.Blocked
	}
	limits.Add("aeroapi", "FlightAware", aeroMeter, aeroLimit)
	if cfg.BoardRefreshIgnored {
		logger.Warn("AIRDASH_BOARD_REFRESH_MINUTES is ignored: the board refresh interval now follows the monthly cap in Settings")
	}
	airlabsLimit := usage.Limit{Enabled: brd != nil, Period: "month"}
	if brd != nil {
		airlabsLimit.Used = func() (int, int, time.Time) {
			st := brd.Stats()
			return st.CallsMonth, st.CallsCap, brd.CapHitAt()
		}
		airlabsLimit.Exhausted = func() bool {
			_, reached := brd.Schedule()
			return reached
		}
		airlabsLimit.RefreshEvery = func() time.Duration {
			iv, _ := brd.Schedule()
			return iv
		}
	}
	limits.Add("airlabs", "AirLabs", airlabsMeter, airlabsLimit)
	freeUsed := func(m *usage.Meter, capFn func() int) func() (int, int, time.Time) {
		return func() (int, int, time.Time) {
			t := m.Totals()
			return t.Calls, capFn(), t.CapHitAt
		}
	}
	limits.Add("adsbdb", "adsbdb", adsbdbMeter, usage.Limit{Enabled: true, PausedUntil: adsbdbGate.PausedUntil,
		ZeroCapIsNone: true, Used: freeUsed(adsbdbMeter, adsbdbCap)})
	limits.Add("hexdb", "hexdb", hexdbMeter, usage.Limit{Enabled: true, ZeroCapIsNone: true, Used: freeUsed(hexdbMeter, hexdbCap)})
	limits.Add("positions", providerLabel(cfg.Provider), posMeter, usage.Limit{Enabled: true, PollS: int(cfg.Poll.Seconds())})
	if cfg.SettingsFile != "" {
		if err := limits.Persist(filepath.Join(filepath.Dir(cfg.SettingsFile), "usage.json")); err != nil {
			logger.Warn("usage counters not loaded", "err", err)
		}
	}
	apiOpts = append(apiOpts, api.WithLimits(limits))
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
	limitsDone := make(chan struct{})
	go func() { limits.Run(logCtx); close(limitsDone) }()
	logDone := make(chan struct{})
	if srcLog != nil {
		go func() { srcLog.Run(logCtx); close(logDone) }()
	} else {
		close(logDone)
	}
	if brd != nil {
		go brd.Run(ctx, func() string { return cur.Settings().BoardAirport })
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
	<-limitsDone
	if srcLog != nil {
		if err := srcLog.Close(); err != nil {
			logger.Warn("route source log close failed", "err", err)
		}
	}
	return shutdownErr
}

// providerLabel names the position feed on the limits page.
func providerLabel(name string) string {
	if name == "adsblol" {
		return "adsb.lol"
	}
	return "adsb.fi"
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
