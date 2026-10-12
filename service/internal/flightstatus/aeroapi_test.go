package flightstatus

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func serve(t *testing.T, status int, file string, gotKey, gotPath, gotQuery *string) *httptest.Server {
	t.Helper()
	var body []byte
	if file != "" {
		var err error
		body, err = os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotKey != nil {
			*gotKey = r.Header.Get("x-apikey")
		}
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		if gotQuery != nil {
			*gotQuery = r.URL.RawQuery
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestStatus_AirborneInstanceIsChosen(t *testing.T) {
	var key, path, query string
	srv := serve(t, 200, "aeroapi_tra39z.json", &key, &path, &query)
	defer srv.Close()
	a := NewAeroAPI(srv.Client(), srv.URL, "secret-key", "airdash/test")
	st, ok, err := a.Status(t.Context(), Query{Callsign: "TRA39Z"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if st != (Status{Kind: KindLate, DelayMin: 7, Basis: "arrival"}) {
		t.Errorf("got %+v", st)
	}
	if key != "secret-key" || path != "/flights/TRA39Z" || query != "max_pages=1" {
		t.Errorf("request wrong: key=%q path=%q query=%q", key, path, query)
	}
}

func TestPickFlight_OnlyLanded(t *testing.T) {
	srv := serve(t, 200, "aeroapi_landed_only.json", nil, nil, nil)
	defer srv.Close()
	_, ok, err := NewAeroAPI(srv.Client(), srv.URL, "k", "ua").Status(t.Context(), Query{Callsign: "TRA39Z"})
	if err != nil || ok {
		t.Errorf("landed-only list must be unknown: ok=%v err=%v", ok, err)
	}
}

func TestStatus_CancelledOnlyIsUnknown(t *testing.T) {
	srv := serve(t, 200, "aeroapi_cancelled.json", nil, nil, nil)
	defer srv.Close()
	st, ok, err := NewAeroAPI(srv.Client(), srv.URL, "k", "ua").Status(t.Context(), Query{Callsign: "VLG1234"})
	if err != nil || ok || st != (Status{}) {
		t.Errorf("got %+v ok=%v err=%v, want unknown", st, ok, err)
	}
}

func TestStatus_NotFoundIsUnknown(t *testing.T) {
	for _, code := range []int{400, 404} {
		srv := serve(t, code, "", nil, nil, nil)
		_, ok, err := NewAeroAPI(srv.Client(), srv.URL, "k", "ua").Status(t.Context(), Query{Callsign: "ZZZ1"})
		srv.Close()
		if err != nil || ok {
			t.Errorf("%d: ok=%v err=%v", code, ok, err)
		}
	}
}

func TestStatus_AuthErrorIsErrAuth(t *testing.T) {
	for _, code := range []int{401, 403} {
		srv := serve(t, code, "", nil, nil, nil)
		_, ok, err := NewAeroAPI(srv.Client(), srv.URL, "bad-key", "ua").Status(t.Context(), Query{Callsign: "TRA39Z"})
		srv.Close()
		if ok || !errors.Is(err, ErrAuth) {
			t.Errorf("%d: ok=%v err=%v, want errors.Is(err, ErrAuth)", code, ok, err)
		}
	}
}

func TestStatus_RateLimitedAndServerErrorAreErrors(t *testing.T) {
	for _, code := range []int{429, 500, 503} {
		srv := serve(t, code, "", nil, nil, nil)
		_, _, err := NewAeroAPI(srv.Client(), srv.URL, "k", "ua").Status(t.Context(), Query{Callsign: "TRA39Z"})
		srv.Close()
		if err == nil {
			t.Errorf("%d: expected error", code)
		}
	}
}

func TestStatus_MalformedBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer srv.Close()
	if _, _, err := NewAeroAPI(srv.Client(), srv.URL, "k", "ua").Status(t.Context(), Query{Callsign: "TRA39Z"}); err == nil {
		t.Error("expected error")
	}
}

func fl(status string, progress int, off, on *string, dep, arr *int, cancelled, diverted bool) flight {
	return flight{Status: status, ProgressPercent: &progress, ActualOff: off, ActualOn: on, DepartureDelay: dep, ArrivalDelay: arr, Cancelled: cancelled, Diverted: diverted}
}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

func TestPickFlight_Fallbacks(t *testing.T) {
	enroute := fl("En Route / Delayed", 0, nil, nil, ip(0), nil, false, false)
	if f, ok := pickFlight([]flight{fl("Arrived", 100, sp("x"), sp("y"), nil, nil, false, false), enroute}); !ok || f.Status != "En Route / Delayed" {
		t.Errorf("status prefix fallback failed: %+v ok=%v", f, ok)
	}
	half := fl("Taxiing", 50, nil, nil, ip(0), nil, false, false)
	if f, ok := pickFlight([]flight{half}); !ok || f.Status != "Taxiing" {
		t.Errorf("progress fallback failed: %+v ok=%v", f, ok)
	}
	if _, ok := pickFlight(nil); ok {
		t.Error("empty list must be unknown")
	}
}

// TestPickFlight_StaleCancelledPlusDepartingIsUnknown: the tracker only
// ever looks up an airborne aircraft, so a cancelled instance in the list
// can only be a stale earlier day's flight, never the one actually in the
// air. Falling through to it (a since-removed tier 4) produced a wrong
// "Cancelled" that then sat in the cache for 15 minutes.
func TestPickFlight_StaleCancelledPlusDepartingIsUnknown(t *testing.T) {
	staleCancelled := fl("Cancelled", 0, nil, nil, nil, nil, true, false)
	notYetDeparted := fl("Scheduled", 0, nil, nil, nil, nil, false, false)
	if _, ok := pickFlight([]flight{staleCancelled, notYetDeparted}); ok {
		t.Error("a stale cancelled instance plus a not-yet-departed one must be unknown")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		f    flight
		want Status
		ok   bool
	}{
		{"on time small delay", fl("En Route", 50, nil, nil, ip(120), ip(240), false, false), Status{KindOnTime, 4, "arrival"}, true},
		{"late arrival", fl("En Route", 50, nil, nil, ip(0), ip(448), false, false), Status{KindLate, 7, "arrival"}, true},
		{"early arrival", fl("En Route", 50, nil, nil, ip(0), ip(-900), false, false), Status{KindEarly, -15, "arrival"}, true},
		{"departure basis", fl("En Route", 5, nil, nil, ip(840), nil, false, false), Status{KindLate, 14, "departure"}, true},
		{"exactly five late", fl("En Route", 5, nil, nil, nil, ip(300), false, false), Status{KindLate, 5, "arrival"}, true},
		{"cancelled", fl("Cancelled", 0, nil, nil, nil, nil, true, false), Status{KindCancelled, 0, ""}, true},
		{"diverted", fl("Diverted", 60, nil, nil, ip(0), ip(0), false, true), Status{KindDiverted, 0, ""}, true},
		{"no delay data", fl("Scheduled", 0, nil, nil, nil, nil, false, false), Status{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(tc.f)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got %+v ok=%v, want %+v ok=%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestClassify_DepartureBasis(t *testing.T) {
	got, ok := classify(fl("En Route", 3, nil, nil, ip(600), nil, false, false))
	if !ok || got.Basis != "departure" || got.Kind != KindLate || got.DelayMin != 10 {
		t.Errorf("got %+v ok=%v", got, ok)
	}
}
