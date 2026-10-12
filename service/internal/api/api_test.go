package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"airdash/internal/flightstatus"
	"airdash/internal/geocode"
	"airdash/internal/settings"
	"airdash/internal/tracker"
)

type fakeSource struct {
	snap tracker.Snapshot
	list []tracker.Snapshot
	at   time.Time
}

func (f *fakeSource) Current() (tracker.Snapshot, time.Time) { return f.snap, f.at }
func (f *fakeSource) List() ([]tracker.Snapshot, time.Time)  { return f.list, f.at }

type fakeStore struct {
	s   settings.Settings
	err error
}

func (f *fakeStore) Settings() settings.Settings { return f.s }
func (f *fakeStore) Set(s settings.Settings) (settings.Settings, error) {
	if f.err != nil {
		return settings.Settings{}, f.err
	}
	n, _, err := settings.Normalize(s)
	if err != nil {
		return settings.Settings{}, err
	}
	f.s = n
	return n, nil
}
func (f *fakeStore) Update(apply func(settings.Settings) settings.Settings) (settings.Settings, error) {
	return f.Set(apply(f.s))
}

func store(radius float64) *fakeStore { return &fakeStore{s: settings.Settings{RadiusKm: radius}} }

type fakeStats struct {
	enabled bool
	used    int
	limit   int
	source  string

	boardAirport                                 string
	boardFlights, boardAge, boardCalls, boardCap int
	boardSource                                  string
}

func (f *fakeStats) Board() (string, int, int, int, int, string) {
	return f.boardAirport, f.boardFlights, f.boardAge, f.boardCalls, f.boardCap, f.boardSource
}

func (f *fakeStats) DelayEnabled() bool            { return f.enabled }
func (f *fakeStats) DelayToday() (used, limit int) { return f.used, f.limit }
func (f *fakeStats) DelaySource() string           { return f.source }

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if ct := rec.Header().Get("Content-Type"); ct == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad json: %v: %s", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestNearest_Found(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 10, 0, time.UTC)
	src := &fakeSource{
		snap: tracker.Snapshot{Found: true, Hex: "400877", Callsign: "BAW466", Type: "A319", Reg: "G-EUPG",
			Airline: "British Airways", AirlineIATA: "BA", Origin: "LHR", Dest: "BCN", OriginName: "London", DestName: "Barcelona",
			AltFt: 3525, GroundKt: 267, Track: 216, DistKm: 3.4, Bearing: 112, Nearby: 3},
		at: now.Add(-2 * time.Second),
	}
	h := NewHandler(src, store(10), &fakeStats{}, nil, func() time.Time { return now })
	rec, body := get(t, h, "/api/nearest")

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	want := map[string]any{
		"found": true, "callsign": "BAW466", "hex": "400877", "index": 0.0, "type": "A319", "reg": "G-EUPG",
		"airline": "British Airways", "airline_iata": "BA", "origin": "LHR", "dest": "BCN",
		"origin_label": "LHR", "dest_label": "BCN", "logo": "",
		"alt_ft": 3525.0, "gs_kt": 267.0, "track": 216.0, "dist_km": 3.4, "bearing": 112.0,
		"age_s": 2.0, "nearby": 3.0, "radius_km": 10.0,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v (%T), want %v", k, body[k], body[k], v)
		}
	}
	if len(body) != len(want) {
		t.Errorf("unexpected extra keys: %v", body)
	}
}

func TestNearest_NotFound(t *testing.T) {
	now := time.Now()
	src := &fakeSource{snap: tracker.Snapshot{Found: false, Nearby: 0}, at: now.Add(-45 * time.Second)}
	h := NewHandler(src, store(12.5), &fakeStats{}, nil, func() time.Time { return now })
	_, body := get(t, h, "/api/nearest")

	if body["found"] != false {
		t.Errorf("found = %v", body["found"])
	}
	if body["age_s"] != 45.0 || body["nearby"] != 0.0 || body["radius_km"] != 12.5 {
		t.Errorf("body = %v", body)
	}
	if _, has := body["callsign"]; has {
		t.Error("aircraft fields must be omitted when not found")
	}
	if len(body) != 4 {
		t.Errorf("want exactly found/age_s/nearby/radius_km, got %v", body)
	}
}

func TestNearest_EmptyRouteFieldsArePresent(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "PRIV1"}, at: time.Now()}
	h := NewHandler(src, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/nearest")
	for _, k := range []string{"airline", "airline_iata", "origin", "dest", "origin_label", "dest_label", "logo"} {
		if v, has := body[k]; !has || v != "" {
			t.Errorf("%s should be present and empty, got %v (has=%v)", k, v, has)
		}
	}
}

func TestNearest_ZeroAltitudeIsPresent(t *testing.T) {
	src := &fakeSource{
		snap: tracker.Snapshot{Found: true, Callsign: "GND2", AltFt: 0},
		at:   time.Now(),
	}
	h := NewHandler(src, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/nearest")
	v, has := body["alt_ft"]
	if !has {
		t.Fatal("alt_ft key must be present when found")
	}
	if v != 0.0 {
		t.Errorf("alt_ft = %v, want 0", v)
	}
}

func TestHealthzAndRoot(t *testing.T) {
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	rec, _ := get(t, h, "/healthz")
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	rec, _ = get(t, h, "/")
	if rec.Code != 200 {
		t.Errorf("root: %d", rec.Code)
	}
	rec, _ = get(t, h, "/nope")
	if rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/nearest", nil))
	if rec.Code != 405 {
		t.Errorf("POST: %d, want 405", rec.Code)
	}
}

func put(t *testing.T, h http.Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestSettings_GetReturnsCurrent(t *testing.T) {
	st := &fakeStore{s: settings.Settings{Airport: "AGP", RadiusKm: 10}}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	rec, body := get(t, h, "/api/settings")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code=%d headers=%v", rec.Code, rec.Header())
	}
	want := map[string]any{"origin": "", "dest": "", "airport": "AGP", "radius_km": 10.0,
		"show_delay": false, "delay_lookups_per_day": 0.0, "show_names": false, "show_logos": false}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v want %v", k, body[k], v)
		}
	}
	if len(body) != 11 {
		t.Errorf("unexpected keys: %v", body)
	}
}

func TestSettings_PutMergesAndNormalizes(t *testing.T) {
	st := &fakeStore{s: settings.Settings{Airport: "AGP", RadiusKm: 10}}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	rec, body := put(t, h, `{"origin":" lhr ","radius_km":25}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if body["origin"] != "LHR" || body["airport"] != "AGP" || body["radius_km"] != 25.0 {
		t.Errorf("merge/normalize wrong: %v", body)
	}
	if st.s.Origin != "LHR" || st.s.Airport != "AGP" || st.s.RadiusKm != 25 {
		t.Errorf("store not updated: %+v", st.s)
	}
}

func TestSettings_PutClearsWithEmptyString(t *testing.T) {
	st := &fakeStore{s: settings.Settings{Airport: "AGP", RadiusKm: 10}}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	rec, body := put(t, h, `{"airport":""}`)
	if rec.Code != 200 || body["airport"] != "" {
		t.Errorf("code=%d body=%v", rec.Code, body)
	}
}

func TestSettings_PutRejectsBadValues(t *testing.T) {
	cases := []string{`{"airport":"TOOLONG"}`, `{"radius_km":0}`, `{"radius_km":-3}`}
	for _, c := range cases {
		st := &fakeStore{s: settings.Settings{RadiusKm: 10}}
		h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
		rec, body := put(t, h, c)
		if rec.Code != 400 || body["error"] == "" {
			t.Errorf("%s: code=%d body=%v", c, rec.Code, body)
		}
		if st.s.RadiusKm != 10 {
			t.Errorf("%s: store changed on invalid input", c)
		}
	}
}

func TestSettings_PutRejectsBadJSON(t *testing.T) {
	cases := []string{`{"radius_km":"ten"}`, `{"foo":1}`, `not json`, `[]`}
	for _, c := range cases {
		st := &fakeStore{s: settings.Settings{RadiusKm: 10}}
		h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
		rec, body := put(t, h, c)
		if rec.Code != 400 || body["error"] == "" {
			t.Errorf("%s: code=%d body=%s", c, rec.Code, rec.Body.String())
		}
	}
}

func TestSettings_PutStoreErrorIs500(t *testing.T) {
	st := &fakeStore{s: settings.Settings{RadiusKm: 10}, err: errors.New("disk full")}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	rec, body := put(t, h, `{"radius_km":5}`)
	if rec.Code != 500 {
		t.Errorf("code=%d", rec.Code)
	}
	if body["error"] != "could not save settings" {
		t.Errorf("error = %v, want generic message", body["error"])
	}
	if strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("internal error leaked to client: %s", rec.Body.String())
	}
}

func TestSettings_MethodNotAllowed(t *testing.T) {
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/settings", nil))
	if rec.Code != 405 {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestNearest_RadiusComesFromSnapshot(t *testing.T) {
	// The snapshot's own radius (set by the tick that produced it) wins
	// over the store's current value, so a settings change mid-flight
	// doesn't relabel an in-flight tick's response with a radius it wasn't
	// actually fetched at.
	src := &fakeSource{snap: tracker.Snapshot{RadiusKm: 42}, at: time.Now()}
	h := NewHandler(src, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/nearest")
	if body["radius_km"] != 42.0 {
		t.Errorf("radius_km = %v, want the snapshot's 42, not the store's 10", body["radius_km"])
	}

	// Before the first tick the snapshot's radius is the zero value; fall
	// back to the store so /api/nearest doesn't report radius_km: 0.
	zero := &fakeSource{snap: tracker.Snapshot{RadiusKm: 0}, at: time.Now()}
	h2 := NewHandler(zero, store(10), &fakeStats{}, nil, time.Now)
	_, body2 := get(t, h2, "/api/nearest")
	if body2["radius_km"] != 10.0 {
		t.Errorf("radius_km = %v, want store fallback 10 when snapshot radius is zero", body2["radius_km"])
	}
}

func TestNearest_DelayIncludedWhenPresent(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "TRA39Z", RadiusKm: 10,
		Delay: &flightstatus.Status{Kind: flightstatus.KindLate, DelayMin: 7, Basis: "arrival"}}, at: time.Now()}
	h := NewHandler(src, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/nearest")
	d, ok := body["delay"].(map[string]any)
	if !ok || d["kind"] != "late" || d["min"] != 7.0 || d["basis"] != "arrival" {
		t.Errorf("delay = %v", body["delay"])
	}
}

func TestNearest_DelayOmittedWhenNil(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "X", RadiusKm: 10}, at: time.Now()}
	h := NewHandler(src, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/nearest")
	if _, has := body["delay"]; has {
		t.Error("delay must be omitted when nil")
	}
}

func TestNearest_CancelledDelayOmitsBasis(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "VLG1234", RadiusKm: 10,
		Delay: &flightstatus.Status{Kind: flightstatus.KindCancelled}}, at: time.Now()}
	h := NewHandler(src, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/nearest")
	d, ok := body["delay"].(map[string]any)
	if !ok || d["kind"] != "cancelled" {
		t.Fatalf("delay = %v", body["delay"])
	}
	if _, has := d["basis"]; has {
		t.Error("basis must be omitted for cancelled/diverted, not serialized as an empty string")
	}
}

func TestSettings_DelayKeysRoundTrip(t *testing.T) {
	st := &fakeStore{s: settings.Settings{RadiusKm: 10, ShowDelay: true, DelayLookupsPerDay: 40}}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/settings")
	if body["show_delay"] != true || body["delay_lookups_per_day"] != 40.0 || len(body) != 11 {
		t.Errorf("GET = %v", body)
	}
	rec, body := put(t, h, `{"show_delay":false,"delay_lookups_per_day":5}`)
	if rec.Code != 200 || body["show_delay"] != false || body["delay_lookups_per_day"] != 5.0 || body["radius_km"] != 10.0 {
		t.Errorf("PUT = %d %v", rec.Code, body)
	}
	rec, _ = put(t, h, `{"delay_lookups_per_day":5000}`)
	if rec.Code != 400 {
		t.Errorf("cap 5000 should be 400, got %d", rec.Code)
	}
}

func TestStats(t *testing.T) {
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{enabled: true, used: 12, limit: 40, source: "aeroapi",
		boardAirport: "AGP", boardFlights: 187, boardAge: 720, boardCalls: 34, boardCap: 900, boardSource: "airlabs"}, nil, time.Now)
	rec, body := get(t, h, "/api/stats")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code=%d", rec.Code)
	}
	want := map[string]any{"delay_enabled": true, "delay_lookups_today": 12.0, "delay_lookups_cap": 40.0, "delay_source": "aeroapi",
		"board_airport": "AGP", "board_flights": 187.0, "board_age_s": 720.0, "board_calls_month": 34.0, "board_calls_cap": 900.0, "board_source": "airlabs"}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v want %v", k, body[k], v)
		}
	}
	if len(body) != 10 {
		t.Errorf("unexpected keys: %v", body)
	}
}

func TestNearest_LabelsUseNamesWhenOn(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "BAW466", Origin: "LHR", Dest: "BCN",
		OriginName: "London", DestName: "Barcelona"}, at: time.Now()}
	st := &fakeStore{s: settings.Settings{RadiusKm: 10, ShowNames: true}}
	_, body := get(t, NewHandler(src, st, &fakeStats{}, nil, time.Now), "/api/nearest")
	if body["origin_label"] != "London" || body["dest_label"] != "Barcelona" || body["origin"] != "LHR" || body["dest"] != "BCN" {
		t.Errorf("got %v", body)
	}
}

func TestNearest_LabelFallsBackToCode(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "X", Origin: "LHR", Dest: "LEMG",
		OriginName: "London", DestName: ""}, at: time.Now()}
	st := &fakeStore{s: settings.Settings{RadiusKm: 10, ShowNames: true}}
	_, body := get(t, NewHandler(src, st, &fakeStats{}, nil, time.Now), "/api/nearest")
	if body["origin_label"] != "London" || body["dest_label"] != "LEMG" {
		t.Errorf("got %v", body)
	}
}

func TestSettings_ShowNamesRoundTrip(t *testing.T) {
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/settings")
	if body["show_names"] != false || len(body) != 11 {
		t.Errorf("GET = %v", body)
	}
	rec, body := put(t, h, `{"show_names":true}`)
	if rec.Code != 200 || body["show_names"] != true || body["radius_km"] != 10.0 {
		t.Errorf("PUT = %d %v", rec.Code, body)
	}
}

func TestSettings_ShowLogosRoundTrip(t *testing.T) {
	st := &fakeStore{s: settings.Settings{RadiusKm: 10, ShowLogos: true}}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/settings")
	if body["show_logos"] != true || len(body) != 11 {
		t.Errorf("GET = %v", body)
	}
	rec, body := put(t, h, `{"show_logos":false}`)
	if rec.Code != 200 || body["show_logos"] != false || body["radius_km"] != 10.0 {
		t.Errorf("PUT = %d %v", rec.Code, body)
	}
}

type fakeLogos struct {
	png   []byte
	codes []string
	err   error
}

func (f *fakeLogos) Get(ctx context.Context, iata string) ([]byte, bool, error) {
	f.codes = append(f.codes, iata)
	if f.err != nil {
		return nil, false, f.err
	}
	if iata == "BA" {
		return f.png, true, nil
	}
	return nil, false, nil
}

func TestLogo_HitHeaders(t *testing.T) {
	lg := &fakeLogos{png: []byte("\x89PNGfake")}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/BA", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" ||
		rec.Header().Get("Cache-Control") != "public, max-age=86400" || rec.Body.String() != "\x89PNGfake" {
		t.Errorf("code=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestLogo_UpperCases(t *testing.T) {
	lg := &fakeLogos{png: []byte("x")}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/ba", nil))
	if rec.Code != 200 || len(lg.codes) != 1 || lg.codes[0] != "BA" {
		t.Errorf("code=%d codes=%v", rec.Code, lg.codes)
	}
}

func TestLogo_MissAndInvalidAre404(t *testing.T) {
	lg := &fakeLogos{png: []byte("x")}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	for _, p := range []string{"/api/logo/ZZ", "/api/logo/BAW", "/api/logo/B%2F"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != 404 || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Errorf("%s: code=%d cc=%q", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	if len(lg.codes) != 1 || lg.codes[0] != "ZZ" {
		t.Errorf("invalid codes must not reach the source: %v", lg.codes)
	}
}

func TestLogo_NilSourceAndErrorAre404(t *testing.T) {
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/BA", nil))
	if rec.Code != 404 {
		t.Errorf("nil source: %d", rec.Code)
	}
	h = NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, &fakeLogos{err: errors.New("down")}, time.Now)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/BA", nil))
	if rec.Code != 404 {
		t.Errorf("error: %d", rec.Code)
	}
}

func TestNearest_LogoFollowsSetting(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{Found: true, Callsign: "X", AirlineIATA: "FR"}, at: time.Now()}
	on := &fakeStore{s: settings.Settings{RadiusKm: 10, ShowLogos: true}}
	_, body := get(t, NewHandler(src, on, &fakeStats{}, nil, time.Now), "/api/nearest")
	if body["logo"] != "FR" {
		t.Errorf("on: %v", body["logo"])
	}
	off := &fakeStore{s: settings.Settings{RadiusKm: 10, ShowLogos: false}}
	_, body = get(t, NewHandler(src, off, &fakeStats{}, nil, time.Now), "/api/nearest")
	if body["logo"] != "" {
		t.Errorf("off: %v", body["logo"])
	}
}

func realPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 96, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 96; x++ {
			img.Set(x, y, color.NRGBA{0, 255, 0, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestLogoBitmap_Hit(t *testing.T) {
	lg := &fakeLogos{png: realPNG(t)}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/ba/24.rgb565", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		rec.Header().Get("Cache-Control") != "public, max-age=86400" || rec.Body.Len() != 1152 {
		t.Fatalf("code=%d headers=%v len=%d", rec.Code, rec.Header(), rec.Body.Len())
	}
	if b := rec.Body.Bytes(); b[0] != 0x07 || b[1] != 0xE0 {
		t.Errorf("green big-endian expected, got %02x%02x", b[0], b[1])
	}
}

func TestLogoBitmap_MissBadPNGAndNil(t *testing.T) {
	cases := []struct {
		name string
		lg   LogoSource
		path string
	}{
		{"miss", &fakeLogos{png: realPNG(t)}, "/api/logo/ZZ/24.rgb565"},
		{"bad png", &fakeLogos{png: []byte("nope")}, "/api/logo/BA/24.rgb565"},
		{"invalid code", &fakeLogos{png: realPNG(t)}, "/api/logo/BAW/24.rgb565"},
	}
	for _, c := range cases {
		h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, c.lg, time.Now)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != 404 || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Errorf("%s: code=%d cc=%q", c.name, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/BA/24.rgb565", nil))
	if rec.Code != 404 {
		t.Errorf("nil source: %d", rec.Code)
	}
}

func three() []tracker.Snapshot {
	mk := func(hex, cs string, i int, d float64) tracker.Snapshot {
		return tracker.Snapshot{Found: true, Hex: hex, Callsign: cs, Index: i, DistKm: d, Nearby: 3, RadiusKm: 10}
	}
	return []tracker.Snapshot{mk("a1", "NEAR", 0, 1.1), mk("b2", "MID", 1, 3.3), mk("c3", "FAR", 2, 6.7)}
}

func nearestWith(t *testing.T, list []tracker.Snapshot, query string) map[string]any {
	t.Helper()
	var cur tracker.Snapshot
	if len(list) > 0 {
		cur = list[0]
		cur.Delay = &flightstatus.Status{Kind: flightstatus.KindOnTime}
		list[0].Delay = cur.Delay
	}
	src := &fakeSource{snap: cur, list: list, at: time.Now()}
	_, body := get(t, NewHandler(src, store(10), &fakeStats{}, nil, time.Now), "/api/nearest"+query)
	return body
}

func TestNearest_HexSelectsAndFallsBack(t *testing.T) {
	if b := nearestWith(t, three(), "?hex=b2"); b["hex"] != "b2" || b["index"] != 1.0 || b["callsign"] != "MID" {
		t.Errorf("hex hit: %v", b)
	}
	if b := nearestWith(t, three(), "?hex=zz"); b["hex"] != "a1" || b["index"] != 0.0 {
		t.Errorf("hex miss must fall back to nearest: %v", b)
	}
	if b := nearestWith(t, three(), "?hex=A1"); b["hex"] != "a1" || b["index"] != 0.0 {
		t.Errorf("case is exact, uppercase misses to nearest: %v", b)
	}
}

func TestNearest_AfterStepsAndWraps(t *testing.T) {
	cases := map[string]string{"?after=a1": "b2", "?after=b2": "c3", "?after=c3": "a1", "?after=": "b2", "?after=zz": "b2", "?after=c3&hex=b2": "a1"}
	for q, want := range cases {
		if b := nearestWith(t, three(), q); b["hex"] != want {
			t.Errorf("%s -> %v, want %s", q, b["hex"], want)
		}
	}
	one := three()[:1]
	one[0].Nearby = 1
	if b := nearestWith(t, one, "?after=a1"); b["hex"] != "a1" || b["index"] != 0.0 {
		t.Errorf("alone: %v", b)
	}
}

func TestNearest_DelayOnlyOnNearest(t *testing.T) {
	if b := nearestWith(t, three(), "?hex=b2"); b["delay"] != nil {
		t.Errorf("index 1 must carry no delay: %v", b["delay"])
	}
	if b := nearestWith(t, three(), ""); b["delay"] == nil {
		t.Errorf("nearest keeps its delay: %v", b)
	}
}

func TestNearest_SelectionWithEmptyList(t *testing.T) {
	src := &fakeSource{snap: tracker.Snapshot{RadiusKm: 10}, at: time.Now()}
	_, body := get(t, NewHandler(src, store(10), &fakeStats{}, nil, time.Now), "/api/nearest?after=a1")
	if body["found"] != false {
		t.Errorf("got %v", body)
	}
}

func TestLogoBitmap_SizesInPath(t *testing.T) {
	lg := &fakeLogos{png: realPNG(t)}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	for path, want := range map[string]int{"/api/logo/BA/48.rgb565": 4608, "/api/logo/BA/32.rgb565": 2048, "/api/logo/BA/24.rgb565": 1152, "/api/logo/BA/64.rgb565": 0, "/api/logo/BA/32.png": 0, "/api/logo/BA/x.rgb565": 0} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if want == 0 {
			if rec.Code != 404 {
				t.Errorf("%s: code=%d, want 404", path, rec.Code)
			}
		} else if rec.Code != 200 || rec.Body.Len() != want || rec.Header().Get("Content-Length") != strconv.Itoa(want) {
			t.Errorf("%s: code=%d len=%d cl=%q, want %d", path, rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"), want)
		}
	}
}

func TestLogoBitmap_WideForms(t *testing.T) {
	lg := &fakeLogos{png: realPNG(t)}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/api/logo/BA/104x32.rgb565"); rec.Code != 200 || rec.Body.Len() != 104*32*2 || rec.Header().Get("Content-Length") != "6656" {
		t.Errorf("rgb565: code=%d len=%d cl=%q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	rec := get("/api/logo/BA/104x32.png")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("png: code=%d headers=%v", rec.Code, rec.Header())
	}
	if img, err := png.Decode(rec.Body); err != nil || img.Bounds().Dx() != 104 || img.Bounds().Dy() != 32 {
		t.Errorf("png decode: err=%v", err)
	}
	for _, p := range []string{"/api/logo/BA/999x1.rgb565", "/api/logo/BA/0x32.png", "/api/logo/BA/104x32.gif", "/api/logo/BA/104x32", "/api/logo/BA/300x300.rgb565"} {
		if rec := get(p); rec.Code != 404 {
			t.Errorf("%s: code=%d, want 404", p, rec.Code)
		}
	}
	if rec := get("/api/logo/BA/48.rgb565"); rec.Code != 200 || rec.Body.Len() != 4608 {
		t.Errorf("legacy square still served: code=%d len=%d", rec.Code, rec.Body.Len())
	}
}

// wideBarPNG is the shape the CDN now sends: a 240x80 canvas with a wide
// red wordmark across the middle.
func wideBarPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 240, 80))
	for y := 30; y < 50; y++ {
		for x := 0; x < 240; x++ {
			img.Set(x, y, color.NRGBA{255, 0, 0, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestLogoBitmap_LegacySquareKeepsAspect(t *testing.T) {
	lg := &fakeLogos{png: wideBarPNG(t)}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, lg, time.Now)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logo/BA/48.rgb565", nil))
	if rec.Code != 200 || rec.Body.Len() != 4608 {
		t.Fatalf("code=%d len=%d", rec.Code, rec.Body.Len())
	}
	bm := rec.Body.Bytes()
	px := func(x, y int) uint16 { i := (y*48 + x) * 2; return uint16(bm[i])<<8 | uint16(bm[i+1]) }
	// A 12:1 bar fitted into 48x48 is a 4 px tall stripe across the middle
	// (rows 22-25). Squashing the 240x80 canvas into the square instead
	// would paint rows 18-29, so rows 20 and 28 tell the two apart.
	if px(24, 24) != 0xF800 || px(24, 20) != 0x0000 || px(24, 28) != 0x0000 {
		t.Errorf("aspect not preserved: centre=%04x row20=%04x row28=%04x", px(24, 24), px(24, 20), px(24, 28))
	}
}

func TestSettings_BoardAirportRoundTrip(t *testing.T) {
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/settings")
	if body["board_airport"] != "" || len(body) != 11 {
		t.Errorf("GET = %v", body)
	}
	rec, body := put(t, h, `{"board_airport":"lemg"}`)
	if rec.Code != 200 || body["board_airport"] != "LEMG" {
		t.Errorf("PUT = %d %v", rec.Code, body)
	}
	if rec, _ := put(t, h, `{"board_airport":"A"}`); rec.Code != 400 {
		t.Errorf("bad code should be 400, got %d", rec.Code)
	}
}

func TestSettings_HomeRoundTripAndValidation(t *testing.T) {
	st := &fakeStore{s: settings.Settings{RadiusKm: 10, Lat: 36.6, Lon: -4.4}}
	h := NewHandler(&fakeSource{at: time.Now()}, st, &fakeStats{}, nil, time.Now)
	_, body := get(t, h, "/api/settings")
	if body["lat"] != 36.6 || body["lon"] != -4.4 {
		t.Errorf("GET = %v", body)
	}
	rec, body := put(t, h, `{"lat":51.47,"lon":-0.45}`)
	if rec.Code != 200 || body["lat"] != 51.47 || body["lon"] != -0.45 || body["radius_km"] != 10.0 {
		t.Errorf("PUT = %d %v", rec.Code, body)
	}
	if rec, _ := put(t, h, `{"lat":95}`); rec.Code != 400 {
		t.Errorf("lat 95 should be 400, got %d", rec.Code)
	}
}

type fakeGeocoder struct {
	res   geocode.Result
	ok    bool
	err   error
	query string
}

func (f *fakeGeocoder) Lookup(ctx context.Context, q string) (geocode.Result, bool, error) {
	f.query = q
	return f.res, f.ok, f.err
}

func TestGeocode_Endpoint(t *testing.T) {
	g := &fakeGeocoder{res: geocode.Result{Lat: 36.68, Lon: -4.5, Name: "Aeropuerto de Málaga"}, ok: true}
	h := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now, WithGeocoder(g))
	rec, body := get(t, h, "/api/geocode?q=Malaga+airport")
	if rec.Code != 200 || body["lat"] != 36.68 || body["lon"] != -4.5 || body["name"] != "Aeropuerto de Málaga" || g.query != "Malaga airport" {
		t.Errorf("code=%d body=%v query=%q", rec.Code, body, g.query)
	}
	g.ok = false
	if rec, _ := get(t, h, "/api/geocode?q=nowhere"); rec.Code != 404 {
		t.Errorf("miss should be 404, got %d", rec.Code)
	}
	g.err = geocode.ErrThrottled
	if rec, _ := get(t, h, "/api/geocode?q=fast"); rec.Code != 429 {
		t.Errorf("throttled should be 429, got %d", rec.Code)
	}
	g.err = errors.New("down")
	if rec, _ := get(t, h, "/api/geocode?q=x+y+z"); rec.Code != 502 {
		t.Errorf("upstream error should be 502, got %d", rec.Code)
	}
	if rec, _ := get(t, h, "/api/geocode?q="); rec.Code != 400 {
		t.Errorf("empty query should be 400, got %d", rec.Code)
	}
	plain := NewHandler(&fakeSource{at: time.Now()}, store(10), &fakeStats{}, nil, time.Now)
	if rec, _ := get(t, plain, "/api/geocode?q=Malaga"); rec.Code != 503 {
		t.Errorf("no geocoder configured should be 503, got %d", rec.Code)
	}
}
