package routes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var hexNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type hexFake struct {
	routes   map[string]string // callsign -> raw JSON body, or "404" / "500"
	airports map[string]string // icao -> raw JSON body, or "404" / "500"
	mu       sync.Mutex
	hits     map[string]int
	ua       string
}

func (f *hexFake) serve(w http.ResponseWriter, r *http.Request, table map[string]string, prefix string) {
	key := strings.TrimPrefix(r.URL.Path, prefix)
	f.mu.Lock()
	f.hits[r.URL.Path]++
	f.ua = r.Header.Get("User-Agent")
	f.mu.Unlock()
	body, ok := table[key]
	switch {
	case !ok || body == "404":
		w.WriteHeader(http.StatusNotFound)
	case body == "500":
		w.WriteHeader(http.StatusInternalServerError)
	default:
		_, _ = w.Write([]byte(body))
	}
}

func newHexFake(t *testing.T, routes, airports map[string]string) (*hexFake, *HexDB) {
	t.Helper()
	f := &hexFake{routes: routes, airports: airports, hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/route/icao/", func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, f.routes, "/api/v1/route/icao/")
	})
	mux.HandleFunc("/api/v1/airport/icao/", func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, f.airports, "/api/v1/airport/icao/")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := NewHexDB(srv.Client(), srv.URL, "airdash/test", func() time.Time { return hexNow })
	return f, h
}

func routeBody(route string, age time.Duration) string {
	return fmt.Sprintf(`{"flight":"X","route":%q,"updatetime":%d}`, route, hexNow.Add(-age).Unix())
}

const (
	day      = 24 * time.Hour
	eghhBody = `{"country_code":"GB","iata":"BOH","icao":"EGHH","airport":"Bournemouth Airport","latitude":50.78,"longitude":-1.8425}`
	lemgBody = `{"country_code":"ES","iata":"AGP","icao":"LEMG","airport":"Malaga Airport","latitude":36.6749,"longitude":-4.49911}`
)

func stdAirports() map[string]string {
	return map[string]string{"EGHH": eghhBody, "LEMG": lemgBody}
}

func TestHexDB_Route(t *testing.T) {
	f, h := newHexFake(t, map[string]string{"RYR5EL": routeBody("EGHH-LEMG", 6*day)}, stdAirports())
	r, ok, err := h.Route(t.Context(), "RYR5EL")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := Route{OriginIATA: "BOH", OriginICAO: "EGHH", DestIATA: "AGP", DestICAO: "LEMG",
		HasCoords: true, OriginLat: 50.78, OriginLon: -1.8425, DestLat: 36.6749, DestLon: -4.49911}
	if r != want {
		t.Errorf("got %+v, want %+v", r, want)
	}
	if f.ua != "airdash/test" {
		t.Errorf("User-Agent = %q", f.ua)
	}
}

func TestHexDB_StaleIsUnknown(t *testing.T) {
	for name, body := range map[string]string{
		"31 days":     routeBody("EGHH-LEMG", 31*day),
		"updatetime0": `{"flight":"X","route":"EGHH-LEMG","updatetime":0}`,
		"missing":     `{"flight":"X","route":"EGHH-LEMG"}`,
	} {
		_, h := newHexFake(t, map[string]string{"RYR5EL": body}, stdAirports())
		_, ok, err := h.Route(t.Context(), "RYR5EL")
		if ok || err != nil {
			t.Errorf("%s: ok=%v err=%v, want unknown", name, ok, err)
		}
	}
}

func TestHexDB_MalformedRouteIsUnknown(t *testing.T) {
	for _, route := range []string{"EGHH-LEMG-GCTS", "EGHH", "", "EGHH-", "EGH-LEMG", "EGHH-LEM1", "-LEMG"} {
		_, h := newHexFake(t, map[string]string{"RYR5EL": routeBody(route, day)}, stdAirports())
		_, ok, err := h.Route(t.Context(), "RYR5EL")
		if ok || err != nil {
			t.Errorf("route %q: ok=%v err=%v, want unknown", route, ok, err)
		}
	}
}

func TestHexDB_RouteNormalisesCaseAndSpace(t *testing.T) {
	_, h := newHexFake(t, map[string]string{"RYR5EL": routeBody(" eghh-lemg ", day)}, stdAirports())
	r, ok, err := h.Route(t.Context(), "RYR5EL")
	if err != nil || !ok || r.OriginICAO != "EGHH" || r.DestICAO != "LEMG" {
		t.Errorf("got %+v ok=%v err=%v", r, ok, err)
	}
}

func TestHexDB_RouteNotFound(t *testing.T) {
	_, h := newHexFake(t, map[string]string{"RYR5EL": "404"}, stdAirports())
	_, ok, err := h.Route(t.Context(), "RYR5EL")
	if ok || err != nil {
		t.Errorf("ok=%v err=%v, want unknown", ok, err)
	}
}

func TestHexDB_ServerError(t *testing.T) {
	_, h := newHexFake(t, map[string]string{"RYR5EL": "500"}, stdAirports())
	if _, _, err := h.Route(t.Context(), "RYR5EL"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHexDB_MalformedBodyIsError(t *testing.T) {
	_, h := newHexFake(t, map[string]string{"RYR5EL": "<html>"}, stdAirports())
	if _, _, err := h.Route(t.Context(), "RYR5EL"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHexDB_AirportNotFoundIsUnknown(t *testing.T) {
	_, h := newHexFake(t, map[string]string{"RYR5EL": routeBody("EGHH-LEMG", day)},
		map[string]string{"EGHH": eghhBody, "LEMG": "404"})
	_, ok, err := h.Route(t.Context(), "RYR5EL")
	if ok || err != nil {
		t.Errorf("ok=%v err=%v, want unknown", ok, err)
	}
}

func TestHexDB_AirportErrorIsError(t *testing.T) {
	_, h := newHexFake(t, map[string]string{"RYR5EL": routeBody("EGHH-LEMG", day)},
		map[string]string{"EGHH": eghhBody, "LEMG": "500"})
	if _, _, err := h.Route(t.Context(), "RYR5EL"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHexDB_AirportMemoised(t *testing.T) {
	f, h := newHexFake(t, map[string]string{
		"RYR5EL": routeBody("EGHH-LEMG", day),
		"RYR9ZZ": routeBody("EGHH-LEMG", day),
	}, stdAirports())
	for _, cs := range []string{"RYR5EL", "RYR9ZZ"} {
		if _, ok, err := h.Route(t.Context(), cs); err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", cs, ok, err)
		}
	}
	if n := f.hits["/api/v1/airport/icao/LEMG"]; n != 1 {
		t.Errorf("LEMG fetched %d times, want 1", n)
	}
}

func TestHexDB_MissingAirportMemoised(t *testing.T) {
	f, h := newHexFake(t, map[string]string{"RYR5EL": routeBody("EGHH-LEMG", day)},
		map[string]string{"EGHH": eghhBody})
	for range 2 {
		if _, ok, err := h.Route(t.Context(), "RYR5EL"); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	}
	if n := f.hits["/api/v1/airport/icao/LEMG"]; n != 1 {
		t.Errorf("LEMG fetched %d times, want 1", n)
	}
}

func TestHexDB_AirportErrorNotMemoised(t *testing.T) {
	f, h := newHexFake(t, map[string]string{"RYR5EL": routeBody("EGHH-LEMG", day)},
		map[string]string{"EGHH": eghhBody, "LEMG": "500"})
	for range 2 {
		_, _, _ = h.Route(t.Context(), "RYR5EL")
	}
	if n := f.hits["/api/v1/airport/icao/LEMG"]; n != 2 {
		t.Errorf("LEMG fetched %d times, want 2", n)
	}
}
