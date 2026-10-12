package settings

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"airdash/internal/config"
	"airdash/internal/routes"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFromConfig(t *testing.T) {
	s := FromConfig(config.Config{Origin: "LHR,LGW", Dest: "", Airport: "AGP", RadiusKm: 12.5})
	want := Settings{Origin: "LHR,LGW", Dest: "", Airport: "AGP", RadiusKm: 12.5, DelayLookupsPerDay: 30, ShowLogos: true}
	if s != want {
		t.Errorf("got %+v want %+v", s, want)
	}
}

func TestNormalize_UpperTrim(t *testing.T) {
	s, f, err := Normalize(Settings{Origin: " agp , lemg ", Dest: "", Airport: "", RadiusKm: 10})
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != "AGP,LEMG" {
		t.Errorf("Origin = %q", s.Origin)
	}
	if !f.Active() {
		t.Error("filter should be active")
	}
	if !f.Match(routes.Route{OriginICAO: "LEMG"}) {
		t.Error("ICAO code should match")
	}
}

func TestNormalize_EmptyIsInactive(t *testing.T) {
	_, f, err := Normalize(Settings{RadiusKm: 5})
	if err != nil || f.Active() {
		t.Errorf("err=%v active=%v", err, f.Active())
	}
}

func TestNormalize_RejectsBadCodes(t *testing.T) {
	for _, in := range []string{"AG", "AIRPORT", "A1P", "AGP,,"} {
		if _, _, err := Normalize(Settings{Airport: in, RadiusKm: 10}); err == nil {
			t.Errorf("Normalize(airport=%q) should fail", in)
		}
	}
}

func TestNormalize_RejectsBadRadius(t *testing.T) {
	for _, r := range []float64{0, -1, math.NaN(), math.Inf(1), 250.01, 1e300} {
		_, _, err := Normalize(Settings{RadiusKm: r})
		if err == nil || !strings.Contains(err.Error(), "radius_km") {
			t.Errorf("radius %v: err=%v", r, err)
		}
	}
	if _, _, err := Normalize(Settings{RadiusKm: 250}); err != nil {
		t.Errorf("radius 250 (ceiling) should be accepted: err=%v", err)
	}
}

func TestNormalize_ErrorsAreValidation(t *testing.T) {
	if _, _, err := Normalize(Settings{Airport: "TOOLONG", RadiusKm: 10}); !errors.Is(err, ErrValidation) {
		t.Errorf("bad code: err=%v, want errors.Is(err, ErrValidation)", err)
	}
	if _, _, err := Normalize(Settings{RadiusKm: 0}); !errors.Is(err, ErrValidation) {
		t.Errorf("radius 0: err=%v, want errors.Is(err, ErrValidation)", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	in := Settings{Origin: "AGP", RadiusKm: 7.5, ShowDelay: true, DelayLookupsPerDay: 40}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, found, err := Load(path, Settings{RadiusKm: 10})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if out != in {
		t.Errorf("got %+v want %+v", out, in)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestLoad_MissingFileIsNotFound(t *testing.T) {
	_, found, err := Load(filepath.Join(t.TempDir(), "none.json"), Settings{RadiusKm: 10})
	if err != nil || found {
		t.Errorf("found=%v err=%v", found, err)
	}
}

func TestLoad_CorruptFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o644)
	if _, _, err := Load(path, Settings{RadiusKm: 10}); err == nil {
		t.Error("corrupt file must return an error")
	}
	_ = os.WriteFile(path, []byte(`{"radius_km":-5}`), 0o644)
	if _, _, err := Load(path, Settings{RadiusKm: 10}); err == nil {
		t.Error("invalid values must return an error")
	}
}

func TestLoad_NormalizesHandEditedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"origin":" agp , lemg","dest":"","airport":"","radius_km":10}`), 0o644)
	s, found, err := Load(path, Settings{RadiusKm: 10})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if s.Origin != "AGP,LEMG" {
		t.Errorf("Origin = %q, want AGP,LEMG", s.Origin)
	}
}

func TestCurrent_SetNormalizesPersistsAndSwaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	c, err := NewCurrent(Settings{RadiusKm: 10}, path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if f, r := c.Get(); f.Active() || r != 10 {
		t.Fatalf("initial: active=%v r=%v", f.Active(), r)
	}
	got, err := c.Set(Settings{Dest: "agp", RadiusKm: 25})
	if err != nil {
		t.Fatal(err)
	}
	if got.Dest != "AGP" || got.RadiusKm != 25 {
		t.Errorf("normalized = %+v", got)
	}
	if f, r := c.Get(); !f.Active() || r != 25 {
		t.Errorf("after set: active=%v r=%v", f.Active(), r)
	}
	if c.Settings() != got {
		t.Errorf("Settings() = %+v", c.Settings())
	}
	saved, found, err := Load(path, Settings{RadiusKm: 10})
	if err != nil || !found || saved != got {
		t.Errorf("persisted = %+v found=%v err=%v", saved, found, err)
	}
}

func TestCurrent_SetInvalidKeepsOld(t *testing.T) {
	c, _ := NewCurrent(Settings{Airport: "AGP", RadiusKm: 10}, "", quiet())
	if _, err := c.Set(Settings{Airport: "TOOLONG", RadiusKm: 10}); err == nil {
		t.Fatal("expected error")
	}
	if c.Settings().Airport != "AGP" {
		t.Errorf("old settings must survive: %+v", c.Settings())
	}
}

func TestCurrent_NoPathIsInMemory(t *testing.T) {
	c, _ := NewCurrent(Settings{RadiusKm: 10}, "", quiet())
	if _, err := c.Set(Settings{RadiusKm: 3}); err != nil {
		t.Fatal(err)
	}
	if _, r := c.Get(); r != 3 {
		t.Errorf("r = %v", r)
	}
}

func TestNewCurrent_RejectsInvalidInitial(t *testing.T) {
	if _, err := NewCurrent(Settings{RadiusKm: 0}, "", quiet()); err == nil {
		t.Error("expected error")
	}
}

// TestCurrent_ConcurrentSetsAgreeWithFile runs many concurrent Sets and
// checks the file on disk always matches the last in-memory swap, i.e. no
// interleaved save+swap ever leaves the two disagreeing.
func TestCurrent_ConcurrentSetsAgreeWithFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	c, err := NewCurrent(Settings{RadiusKm: 1}, path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Set(Settings{RadiusKm: float64(i + 1)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	saved, found, err := Load(path, Settings{RadiusKm: 10})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if saved != c.Settings() {
		t.Errorf("file %+v does not match in-memory %+v", saved, c.Settings())
	}
}

// TestCurrent_UpdateMergesUnderLock runs concurrent partial updates (each
// setting a different field off the *current* value) and checks neither
// update is lost, i.e. Update's read-modify-write is atomic under
// concurrency, not a plain read-then-Set that could race with another
// Update's write in between.
func TestCurrent_UpdateMergesUnderLock(t *testing.T) {
	c, err := NewCurrent(Settings{RadiusKm: 10}, "", quiet())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var apply func(Settings) Settings
			if i%2 == 0 {
				apply = func(s Settings) Settings { s.Origin = "AGP"; return s }
			} else {
				apply = func(s Settings) Settings { s.Dest = "LHR"; return s }
			}
			if _, err := c.Update(apply); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	final := c.Settings()
	if final.Origin != "AGP" || final.Dest != "LHR" {
		t.Errorf("lost update: final=%+v, want both Origin=AGP and Dest=LHR set", final)
	}
}

func TestFromConfig_DelayDefaults(t *testing.T) {
	s := FromConfig(config.Config{RadiusKm: 10, AeroAPIKey: "k"})
	if !s.ShowDelay || s.DelayLookupsPerDay != 30 {
		t.Errorf("with key: %+v", s)
	}
	s = FromConfig(config.Config{RadiusKm: 10})
	if s.ShowDelay || s.DelayLookupsPerDay != 30 {
		t.Errorf("without key: %+v", s)
	}
}

func TestNormalize_DelayCapRange(t *testing.T) {
	for _, bad := range []int{-1, 1001} {
		if _, _, err := Normalize(Settings{RadiusKm: 10, DelayLookupsPerDay: bad}); err == nil || !strings.Contains(err.Error(), "delay_lookups_per_day") {
			t.Errorf("cap %d: err=%v", bad, err)
		}
	}
	for _, good := range []int{0, 40, 1000} {
		if _, _, err := Normalize(Settings{RadiusKm: 10, DelayLookupsPerDay: good}); err != nil {
			t.Errorf("cap %d: %v", good, err)
		}
	}
}

func TestLoad_MissingNewKeysUseDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"origin":"","dest":"","airport":"AGP","radius_km":50}`), 0o644)
	s, found, err := Load(path, Settings{RadiusKm: 10, ShowDelay: true, DelayLookupsPerDay: 40})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if s.Airport != "AGP" || s.RadiusKm != 50 || !s.ShowDelay || s.DelayLookupsPerDay != 40 {
		t.Errorf("got %+v", s)
	}
	_ = os.WriteFile(path, []byte(`{"radius_km":50,"show_delay":false,"delay_lookups_per_day":5}`), 0o644)
	s, _, _ = Load(path, Settings{RadiusKm: 10, ShowDelay: true, DelayLookupsPerDay: 40})
	if s.ShowDelay || s.DelayLookupsPerDay != 5 {
		t.Errorf("explicit values must win: %+v", s)
	}
}

func TestCurrent_DelayWanted(t *testing.T) {
	c, _ := NewCurrent(Settings{RadiusKm: 10, ShowDelay: true, DelayLookupsPerDay: 40}, "", quiet())
	if !c.DelayWanted() {
		t.Error("want true")
	}
	_, _ = c.Set(Settings{RadiusKm: 10, ShowDelay: true, DelayLookupsPerDay: 0})
	if c.DelayWanted() {
		t.Error("cap 0 → false")
	}
	_, _ = c.Set(Settings{RadiusKm: 10, ShowDelay: false, DelayLookupsPerDay: 40})
	if c.DelayWanted() {
		t.Error("show_delay false → false")
	}
}

func TestNormalize_KeepsShowNames(t *testing.T) {
	n, _, err := Normalize(Settings{RadiusKm: 10, ShowNames: true})
	if err != nil || !n.ShowNames {
		t.Errorf("got %+v err=%v", n, err)
	}
}

func TestLoad_ShowNamesDefaultsFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"radius_km":50}`), 0o644)
	s, _, err := Load(path, Settings{RadiusKm: 10})
	if err != nil || s.ShowNames {
		t.Errorf("missing key must stay false: %+v err=%v", s, err)
	}
	_ = os.WriteFile(path, []byte(`{"radius_km":50,"show_names":true}`), 0o644)
	s, _, _ = Load(path, Settings{RadiusKm: 10})
	if !s.ShowNames {
		t.Errorf("explicit true must win: %+v", s)
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	s, _, _ = Load(path, Settings{RadiusKm: 10})
	if !s.ShowNames {
		t.Errorf("did not survive save/load: %+v", s)
	}
}

func TestShowLogos_DefaultsTrueAndRoundTrips(t *testing.T) {
	if s := FromConfig(config.Config{RadiusKm: 10}); !s.ShowLogos {
		t.Errorf("default must be on: %+v", s)
	}
	n, _, err := Normalize(Settings{RadiusKm: 10, ShowLogos: true})
	if err != nil || !n.ShowLogos {
		t.Errorf("normalize dropped it: %+v err=%v", n, err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"radius_km":50}`), 0o644)
	s, _, _ := Load(path, Settings{RadiusKm: 10, ShowLogos: true})
	if !s.ShowLogos {
		t.Errorf("missing key must keep the default: %+v", s)
	}
	_ = os.WriteFile(path, []byte(`{"radius_km":50,"show_logos":false}`), 0o644)
	s, _, _ = Load(path, Settings{RadiusKm: 10, ShowLogos: true})
	if s.ShowLogos {
		t.Errorf("explicit false must win: %+v", s)
	}
}

func TestBoardAirport_NormalizesAndValidates(t *testing.T) {
	n, _, err := Normalize(Settings{RadiusKm: 10, BoardAirport: " agp "})
	if err != nil || n.BoardAirport != "AGP" {
		t.Errorf("got %+v err=%v", n, err)
	}
	if _, _, err := Normalize(Settings{RadiusKm: 10, BoardAirport: "AG"}); !errors.Is(err, ErrValidation) {
		t.Errorf("2 chars must fail validation: %v", err)
	}
	if _, _, err := Normalize(Settings{RadiusKm: 10, BoardAirport: "LEMG"}); err != nil {
		t.Errorf("icao ok: %v", err)
	}
	if s := FromConfig(config.Config{RadiusKm: 10, BoardAirport: "AGP"}); s.BoardAirport != "AGP" {
		t.Errorf("FromConfig: %+v", s)
	}
}

func TestFromConfig_ShowDelayOnWithBoardOnly(t *testing.T) {
	if s := FromConfig(config.Config{RadiusKm: 10, AirLabsKey: "k"}); !s.ShowDelay {
		t.Errorf("a board-only setup must show delays by default: %+v", s)
	}
}

func TestHome_FromConfigNormalizeAndValidate(t *testing.T) {
	s := FromConfig(config.Config{Lat: 36.6, Lon: -4.4, RadiusKm: 10})
	if s.Lat != 36.6 || s.Lon != -4.4 {
		t.Errorf("FromConfig: %+v", s)
	}
	n, _, err := Normalize(Settings{RadiusKm: 10, Lat: 36.6, Lon: -4.4})
	if err != nil || n.Lat != 36.6 || n.Lon != -4.4 {
		t.Errorf("normalize: %+v err=%v", n, err)
	}
	for _, bad := range []Settings{{RadiusKm: 10, Lat: 91, Lon: 0}, {RadiusKm: 10, Lat: 0, Lon: -181}, {RadiusKm: 10, Lat: math.NaN(), Lon: 0}} {
		if _, _, err := Normalize(bad); !errors.Is(err, ErrValidation) {
			t.Errorf("%+v must fail validation: %v", bad, err)
		}
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"radius_km":50}`), 0o644)
	if s, _, err := Load(path, Settings{RadiusKm: 10, Lat: 36.6, Lon: -4.4}); err != nil || s.Lat != 36.6 {
		t.Errorf("missing keys keep the configured home: %+v err=%v", s, err)
	}
}
