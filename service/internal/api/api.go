// Package api serves the snapshot to the display. The JSON is flat and
// small on purpose: the ESP32 parses it into a fixed 1536-byte document.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airdash/internal/flightstatus"
	"airdash/internal/geocode"
	"airdash/internal/logos"
	"airdash/internal/settings"
	"airdash/internal/tracker"
)

type Source interface {
	Current() (tracker.Snapshot, time.Time)
	List() ([]tracker.Snapshot, time.Time)
}

type SettingsStore interface {
	Settings() settings.Settings
	Set(settings.Settings) (settings.Settings, error)
	Update(func(settings.Settings) settings.Settings) (settings.Settings, error)
}

// StatsSource answers /api/stats from the delay lookup budget so the
// settings page can show today's usage without exposing the budget type.
type StatsSource interface {
	DelayEnabled() bool
	DelayToday() (used, limit int)
	DelaySource() string
	// Board reports the airport board feeding delay status: airport ("" when
	// off), rows, age in seconds (-1 before the first fetch), this month's
	// calls and cap, and the source name ("" when no board is configured).
	Board() (airport string, flights, ageS, callsMonth, callsCap int, source string)
}

// LogoSource serves airline logos by IATA code; nil disables the endpoint.
type LogoSource interface {
	Get(ctx context.Context, iata string) ([]byte, bool, error)
}

type handler struct {
	src   Source
	st    SettingsStore
	stats StatsSource
	logos LogoSource
	now   func() time.Time

	geocoder geocode.Lookup // nil = address lookup disabled
}

// Option configures optional handler dependencies.
type Option func(*handler)

// WithGeocoder enables GET /api/geocode for the settings page's address
// lookup; without it the route answers 503.
func WithGeocoder(g geocode.Lookup) Option { return func(h *handler) { h.geocoder = g } }

func NewHandler(src Source, st SettingsStore, stats StatsSource, logos LogoSource, now func() time.Time, opts ...Option) http.Handler {
	h := &handler{src: src, st: st, stats: stats, logos: logos, now: now}
	for _, o := range opts {
		o(h)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/geocode", h.getGeocode)
	mux.HandleFunc("GET /api/nearest", h.nearest)
	mux.HandleFunc("GET /api/logo/{iata}", h.getLogo)
	mux.HandleFunc("GET /api/logo/{iata}/{file}", h.getLogoBitmap)
	mux.HandleFunc("GET /api/settings", h.getSettings)
	mux.HandleFunc("PUT /api/settings", h.putSettings)
	mux.HandleFunc("GET /api/stats", h.getStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("airdash: GET /api/nearest for the closest aircraft, GET/PUT /api/settings for filters, GET /api/stats for delay lookup usage\n"))
	})
	return mux
}

// Two response shapes so that a legitimate zero (altitude 0 ft) is never
// dropped by omitempty while the not-found case stays minimal.
type foundResponse struct {
	Found       bool   `json:"found"`
	Callsign    string `json:"callsign"`
	Hex         string `json:"hex"`
	Type        string `json:"type"`
	Reg         string `json:"reg"`
	Airline     string `json:"airline"`
	AirlineIATA string `json:"airline_iata"`
	Origin      string `json:"origin"`
	Dest        string `json:"dest"`
	// What the route row should render: the city when show_names is on
	// and known, otherwise the code. Resolved here, at request time, so a
	// toggle shows on the next poll of either client.
	OriginLabel string `json:"origin_label"`
	DestLabel   string `json:"dest_label"`
	// Logo is the airline code whose logo both clients should draw, or ""
	// when show_logos is off or the airline is unknown. Resolved here so the
	// tickbox reaches the display on its next poll.
	Logo     string  `json:"logo"`
	AltFt    int     `json:"alt_ft"`
	GsKt     int     `json:"gs_kt"`
	Track    int     `json:"track"`
	DistKm   float64 `json:"dist_km"`
	Bearing  int     `json:"bearing"`
	AgeS     int     `json:"age_s"`
	Nearby   int     `json:"nearby"`
	Index    int     `json:"index"`
	RadiusKm float64 `json:"radius_km"`
	// Delay is a pointer (not a value with omitempty on its own fields) so
	// that an absent lookup omits the whole object rather than serializing
	// a misleading zero-value Status.
	Delay *flightstatus.Status `json:"delay,omitempty"`
}

// pick resolves the client's selection against the sorted list. after
// (when present) wins: it steps to the aircraft following that hex,
// wrapping past the end, and an unknown or empty value starts from the
// nearest. hex follows one aircraft while it stays in range and falls
// back to the nearest when it is gone.
func pick(list []tracker.Snapshot, hex, after string, useAfter bool) (tracker.Snapshot, bool) {
	if len(list) == 0 {
		return tracker.Snapshot{}, false
	}
	if useAfter {
		i := 0
		for j, s := range list {
			if s.Hex == after {
				i = j
				break
			}
		}
		return list[(i+1)%len(list)], true
	}
	for _, s := range list {
		if s.Hex == hex {
			return s, true
		}
	}
	return list[0], true
}

func logoCode(iata string, show bool) string {
	if show && logos.ValidCode(iata) {
		return iata
	}
	return ""
}

func label(code, name string, names bool) string {
	if names && name != "" {
		return name
	}
	return code
}

type emptyResponse struct {
	Found    bool    `json:"found"`
	AgeS     int     `json:"age_s"`
	Nearby   int     `json:"nearby"`
	RadiusKm float64 `json:"radius_km"`
}

func (h *handler) nearest(w http.ResponseWriter, r *http.Request) {
	snap, at := h.src.Current()
	if q := r.URL.Query(); q.Get("hex") != "" || q.Has("after") {
		if list, lat := h.src.List(); len(list) > 0 {
			if sel, ok := pick(list, q.Get("hex"), q.Get("after"), q.Has("after")); ok {
				snap, at = sel, lat
			}
		}
	}
	age := int(h.now().Sub(at).Seconds())
	if age < 0 {
		age = 0
	}
	// The snapshot carries the radius its own tick actually fetched at, so
	// a settings change mid-flight doesn't relabel an in-flight tick's
	// response with a radius it wasn't fetched at. Before the first tick
	// the snapshot's radius is the zero value; fall back to the store.
	radius := snap.RadiusKm
	if radius == 0 {
		radius = h.st.Settings().RadiusKm
	}
	var body any
	if snap.Found {
		st := h.st.Settings()
		body = foundResponse{
			Found: true, Callsign: snap.Callsign, Hex: snap.Hex, Index: snap.Index, Type: snap.Type, Reg: snap.Reg,
			Airline: snap.Airline, AirlineIATA: snap.AirlineIATA, Origin: snap.Origin, Dest: snap.Dest,
			OriginLabel: label(snap.Origin, snap.OriginName, st.ShowNames),
			DestLabel:   label(snap.Dest, snap.DestName, st.ShowNames),
			Logo:        logoCode(snap.AirlineIATA, st.ShowLogos),
			AltFt:       snap.AltFt, GsKt: snap.GroundKt, Track: snap.Track,
			DistKm: snap.DistKm, Bearing: snap.Bearing,
			AgeS: age, Nearby: snap.Nearby, RadiusKm: radius,
			Delay: snap.Delay,
		}
	} else {
		body = emptyResponse{Found: false, AgeS: age, Nearby: snap.Nearby, RadiusKm: radius}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

// settingsPatch uses pointers so a missing key keeps the current value
// while an explicit "" clears a list.
type settingsPatch struct {
	Lat                *float64 `json:"lat"`
	Lon                *float64 `json:"lon"`
	Origin             *string  `json:"origin"`
	Dest               *string  `json:"dest"`
	Airport            *string  `json:"airport"`
	RadiusKm           *float64 `json:"radius_km"`
	ShowDelay          *bool    `json:"show_delay"`
	DelayLookupsPerDay *int     `json:"delay_lookups_per_day"`
	ShowNames          *bool    `json:"show_names"`
	ShowLogos          *bool    `json:"show_logos"`
	BoardAirport       *string  `json:"board_airport"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handler) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.st.Settings())
}

func (h *handler) putSettings(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	var p settingsPatch
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Update reads and merges the patch onto the current settings as one
	// atomic step, so a PUT that only changes e.g. origin can't race with
	// a concurrent PUT changing dest and silently drop one of them (each
	// only ever saw the settings as of its own read, not the other's write).
	n, err := h.st.Update(func(cur settings.Settings) settings.Settings {
		if p.Lat != nil {
			cur.Lat = *p.Lat
		}
		if p.Lon != nil {
			cur.Lon = *p.Lon
		}
		if p.Origin != nil {
			cur.Origin = *p.Origin
		}
		if p.Dest != nil {
			cur.Dest = *p.Dest
		}
		if p.Airport != nil {
			cur.Airport = *p.Airport
		}
		if p.RadiusKm != nil {
			cur.RadiusKm = *p.RadiusKm
		}
		if p.ShowDelay != nil {
			cur.ShowDelay = *p.ShowDelay
		}
		if p.DelayLookupsPerDay != nil {
			cur.DelayLookupsPerDay = *p.DelayLookupsPerDay
		}
		if p.ShowNames != nil {
			cur.ShowNames = *p.ShowNames
		}
		if p.ShowLogos != nil {
			cur.ShowLogos = *p.ShowLogos
		}
		if p.BoardAirport != nil {
			cur.BoardAirport = *p.BoardAirport
		}
		return cur
	})
	if err != nil {
		if errors.Is(err, settings.ErrValidation) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			// Don't echo the internal error (e.g. a filesystem path) to the
			// client; log it for the operator instead.
			slog.Default().Error("save settings failed", "err", err)
			writeError(w, http.StatusInternalServerError, "could not save settings")
		}
		return
	}
	writeJSON(w, http.StatusOK, n)
}

type statsResponse struct {
	DelayEnabled      bool   `json:"delay_enabled"`
	DelayLookupsToday int    `json:"delay_lookups_today"`
	DelayLookupsCap   int    `json:"delay_lookups_cap"`
	DelaySource       string `json:"delay_source"`
	BoardAirport      string `json:"board_airport"`
	BoardFlights      int    `json:"board_flights"`
	BoardAgeS         int    `json:"board_age_s"`
	BoardCallsMonth   int    `json:"board_calls_month"`
	BoardCallsCap     int    `json:"board_calls_cap"`
	BoardSource       string `json:"board_source"`
}

func (h *handler) getStats(w http.ResponseWriter, r *http.Request) {
	used, limit := h.stats.DelayToday()
	airport, flights, age, calls, cap, source := h.stats.Board()
	writeJSON(w, http.StatusOK, statsResponse{
		DelayEnabled: h.stats.DelayEnabled(), DelayLookupsToday: used, DelayLookupsCap: limit, DelaySource: h.stats.DelaySource(),
		BoardAirport: airport, BoardFlights: flights, BoardAgeS: age, BoardCallsMonth: calls, BoardCallsCap: cap, BoardSource: source,
	})
}

// logoPNG resolves a request's airline code to cached PNG bytes; ok is
// false for every "no logo" outcome, and the 404 headers are already set.
// Misses are cacheable too: a browser polling every 3 s must not re-ask
// for a logo the CDN does not have.
func (h *handler) logoPNG(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	code := strings.ToUpper(r.PathValue("iata"))
	miss := func() bool {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.Error(w, "no logo", http.StatusNotFound)
		return false
	}
	if h.logos == nil || !logos.ValidCode(code) {
		return nil, miss()
	}
	png, ok, err := h.logos.Get(r.Context(), code)
	if err != nil {
		slog.Default().Warn("logo fetch failed", "iata", code, "err", err)
	}
	if !ok {
		return nil, miss()
	}
	return png, true
}

func (h *handler) getLogo(w http.ResponseWriter, r *http.Request) {
	png, ok := h.logoPNG(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(png)
}

// The display has no image decoder, so it gets a ready-made bitmap. The
// file name carries the box: WxH.rgb565 (cropped to the wordmark and
// fitted) or WxH.png (the same image for the web mirror), plus the older
// square form N.rgb565 for firmware that still asks for it.
func (h *handler) getLogoBitmap(w http.ResponseWriter, r *http.Request) {
	miss := func(msg string) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.Error(w, msg, http.StatusNotFound)
	}
	file := r.PathValue("file")
	var bw, bh, size int
	var suffix string
	wide := false
	if n, _ := fmt.Sscanf(file, "%dx%d.%s", &bw, &bh, &suffix); n == 3 {
		if bw < 1 || bw > 240 || bh < 1 || bh > 135 || (suffix != "rgb565" && suffix != "png") {
			miss("no such bitmap")
			return
		}
		wide = true
	} else if n, _ := fmt.Sscanf(file, "%d.%s", &size, &suffix); n != 2 || suffix != "rgb565" || !logos.ValidBitmapSize(size) {
		miss("no such bitmap")
		return
	}
	png, ok := h.logoPNG(w, r)
	if !ok {
		return
	}
	var body []byte
	var err error
	ctype := "application/octet-stream"
	switch {
	case wide && suffix == "png":
		var img *image.RGBA
		if img, err = logos.Fit(png, bw, bh); err == nil {
			body, err = logos.EncodePNG(img)
		}
		ctype = "image/png"
	case wide:
		var img *image.RGBA
		if img, err = logos.Fit(png, bw, bh); err == nil {
			body = logos.EncodeRGB565(img)
		}
	default:
		// Older firmware asks for a square; fit rather than stretch so the
		// wordmark keeps its shape (letterboxed on black).
		var img *image.RGBA
		if img, err = logos.Fit(png, size, size); err == nil {
			body = logos.EncodeRGB565(img)
		}
	}
	if err != nil {
		slog.Default().Warn("logo convert failed", "iata", r.PathValue("iata"), "err", err)
		miss("no logo")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// Explicit: the firmware rejects a body without a Content-Length, and
	// net/http switches to chunked encoding just above 2048 bytes.
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// getGeocode resolves a typed address for the page. Errors map to codes
// the page can explain: 429 for the one-per-second policy, 502 for an
// upstream failure, 503 when no geocoder is configured.
func (h *handler) getGeocode(w http.ResponseWriter, r *http.Request) {
	if h.geocoder == nil {
		writeError(w, http.StatusServiceUnavailable, "address lookup not configured")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 3 {
		writeError(w, http.StatusBadRequest, "type at least 3 characters")
		return
	}
	res, ok, err := h.geocoder.Lookup(r.Context(), q)
	switch {
	case errors.Is(err, geocode.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, "one lookup per second, try again")
		return
	case err != nil:
		slog.Default().Warn("geocode failed", "err", err)
		writeError(w, http.StatusBadGateway, "address lookup failed")
		return
	case !ok:
		writeError(w, http.StatusNotFound, "address not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lat": res.Lat, "lon": res.Lon, "name": res.Name})
}
