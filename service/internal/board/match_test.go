package board

import (
	"testing"
	"time"

	"airdash/internal/flightstatus"
)

func at(h, m int) time.Time { return time.Date(2026, 9, 29, h, m, 0, 0, time.UTC) }

func deps(t *testing.T) []Row {
	rows, _ := ParseAirLabs(fixture(t, "airlabs_dep_agp.json"))
	return rows
}
func arrs(t *testing.T) []Row {
	rows, _ := ParseAirLabs(fixture(t, "airlabs_arr_agp.json"))
	return rows
}

func TestMatch_ByCallsign(t *testing.T) {
	r, ok := Match(deps(t), "AGP", flightstatus.Query{Callsign: "wmt6026", AirlineIATA: "W4", Origin: "AGP", Dest: "FCO"}, at(13, 30))
	if !ok || r.FlightICAO != "WMT6026" {
		t.Fatalf("ok=%v row=%+v", ok, r)
	}
	st := StatusOf(r, false, time.Time{})
	if st.Kind != flightstatus.KindLate || st.DelayMin != 9 || st.Basis != "departure" {
		t.Errorf("status = %+v", st)
	}
}

func TestMatch_ByCallsignWithoutAirline(t *testing.T) {
	r, ok := Match(arrs(t), "AGP", flightstatus.Query{Callsign: "HTY251", Origin: "JCU", Dest: "AGP"}, at(13, 40))
	if !ok || r.FlightICAO != "HTY251" {
		t.Fatalf("ok=%v row=%+v", ok, r)
	}
	if st := StatusOf(r, true, time.Time{}); st.Kind != flightstatus.KindLate || st.DelayMin != 9 || st.Basis != "arrival" {
		t.Errorf("status = %+v", st)
	}
}

func TestMatch_DepartureByAirlineAndFarAirport(t *testing.T) {
	// Ryanair flies a scrambled callsign; the board row is RYR4687 AGP→ARN,
	// actual departure 13:24. Early by 6 min (delay field is null).
	q := flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "ARN"}
	r, ok := Match(deps(t), "AGP", q, at(13, 30))
	if !ok || r.FlightICAO != "RYR4687" {
		t.Fatalf("ok=%v row=%+v", ok, r)
	}
	if st := StatusOf(r, false, time.Time{}); st.Kind != flightstatus.KindEarly || st.DelayMin != -6 {
		t.Errorf("status = %+v", st)
	}
	if _, ok := Match(deps(t), "AGP", q, at(12, 0)); ok {
		t.Error("outside the window must miss")
	}
	if _, ok := Match(deps(t), "AGP", flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "STN"}, at(13, 30)); ok {
		t.Error("wrong far airport must miss")
	}
}

func TestMatch_ArrivalClosestWins(t *testing.T) {
	// RYR8130 DUB→AGP estimated 15:18 (early 2 → on time).
	q := flightstatus.Query{Callsign: "RYR1XY", AirlineIATA: "FR", Origin: "DUB", Dest: "AGP"}
	r, ok := Match(arrs(t), "AGP", q, at(15, 0))
	if !ok || r.FlightICAO != "RYR8130" {
		t.Fatalf("ok=%v row=%+v", ok, r)
	}
	if st := StatusOf(r, true, time.Time{}); st.Kind != flightstatus.KindOnTime || st.DelayMin != -2 {
		t.Errorf("status = %+v", st)
	}
}

func TestMatch_ICAOFarAirport(t *testing.T) {
	q := flightstatus.Query{Callsign: "RYR1XY", AirlineIATA: "FR", Origin: "EIDW", Dest: "LEMG"}
	if r, ok := Match(arrs(t), "LEMG", q, at(15, 0)); !ok || r.FlightICAO != "RYR8130" {
		t.Errorf("icao codes must match: ok=%v row=%+v", ok, r)
	}
}

func TestMatch_CallsignIsAuthoritative(t *testing.T) {
	if r, ok := Match(deps(t), "AGP", flightstatus.Query{Callsign: "RYR4687", AirlineIATA: "FR", Origin: "MAD", Dest: "ARN"}, at(13, 30)); !ok || r.FlightICAO != "RYR4687" {
		t.Errorf("callsign must win over the route: ok=%v", ok)
	}
	if _, ok := Match(deps(t), "AGP", flightstatus.Query{Callsign: "XXX1", AirlineIATA: "FR", Origin: "MAD", Dest: "ARN"}, at(13, 30)); ok {
		t.Error("a route not touching the airport cannot match by airline")
	}
}

func TestStatusOf_Cancelled(t *testing.T) {
	r, ok := Match(arrs(t), "AGP", flightstatus.Query{Callsign: "IBE5834"}, at(15, 0))
	if !ok {
		t.Fatal("cancelled rows still match by callsign")
	}
	if st := StatusOf(r, true, time.Time{}); st.Kind != flightstatus.KindCancelled || st.Basis != "" || st.DelayMin != 0 {
		t.Errorf("status = %+v", st)
	}
}

func row(icao, airline, dep, arr string, depActual, arrEst, arrActual time.Time) Row {
	return Row{FlightICAO: icao, AirlineIATA: airline, DepIATA: dep, ArrIATA: arr, DepActual: depActual, DepSched: depActual, ArrEst: arrEst, ArrSched: arrEst, ArrActual: arrActual, Status: "active"}
}

func TestMatch_CallsignPrefersTheAgreeingDirection(t *testing.T) {
	// A tag-on keeps one flight number: X→AGP (arrival) then AGP→Y (departure).
	rows := []Row{
		row("VLG1234", "VY", "AGP", "BCN", at(15, 40), at(17, 0), time.Time{}),
		row("VLG1234", "VY", "BCN", "AGP", at(13, 0), at(14, 30), time.Time{}),
	}
	r, ok := Match(rows, "AGP", flightstatus.Query{Callsign: "VLG1234", AirlineIATA: "VY", Origin: "BCN", Dest: "AGP"}, at(14, 20))
	if !ok || r.DepIATA != "BCN" {
		t.Errorf("inbound aircraft must match the arrival row: ok=%v row=%+v", ok, r)
	}
	// A callsign row hours away in time is not this aircraft.
	old := []Row{row("VLG9999", "VY", "AGP", "BCN", at(2, 0), at(3, 30), at(3, 25))}
	if _, ok := Match(old, "AGP", flightstatus.Query{Callsign: "VLG9999", AirlineIATA: "VY", Origin: "AGP", Dest: "BCN"}, at(14, 20)); ok {
		t.Error("a callsign row more than 6 h away must not match")
	}
}

func TestMatch_ArrivalPrefersTheOneStillAirborne(t *testing.T) {
	rows := []Row{
		row("RYR1AB", "FR", "DUB", "AGP", at(11, 0), at(14, 52), at(14, 52)),   // landed 8 min ago
		row("RYR2CD", "FR", "DUB", "AGP", at(11, 20), at(15, 12), time.Time{}), // on approach
	}
	r, ok := Match(rows, "AGP", flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "DUB", Dest: "AGP"}, at(15, 0))
	if !ok || r.FlightICAO != "RYR2CD" {
		t.Errorf("the airborne one must win over a row landed minutes ago: ok=%v row=%+v", ok, r)
	}
}

func TestMatch_DeparturePrefersTheOneAlreadyOff(t *testing.T) {
	rows := []Row{
		row("RYR3EF", "FR", "AGP", "STN", time.Time{}, at(17, 0), time.Time{}), // not yet departed, due +3 min
		row("RYR4GH", "FR", "AGP", "STN", at(14, 46), at(17, 30), time.Time{}), // took off 14 min ago
	}
	rows[0].DepSched = at(15, 3)
	r, ok := Match(rows, "AGP", flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "STN"}, at(15, 0))
	if !ok || r.FlightICAO != "RYR4GH" {
		t.Errorf("an aircraft seen airborne is the one that has departed: ok=%v row=%+v", ok, r)
	}
}

func TestMatch_AmbiguousCandidatesAreAMiss(t *testing.T) {
	// Two Ryanair departures to the same destination, both airborne within
	// the window: a guess would attach the wrong delay and block the paid
	// fallback, so the heuristic must fail closed.
	rows := []Row{
		row("RYR1AB", "FR", "AGP", "STN", at(14, 40), at(17, 0), time.Time{}),
		row("RYR2CD", "FR", "AGP", "STN", at(14, 50), at(17, 10), time.Time{}),
	}
	if _, ok := Match(rows, "AGP", flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "STN"}, at(15, 0)); ok {
		t.Error("two equally plausible rows must be a miss")
	}
}

func TestStatusOf_DepartureEstimatedFromFirstSighting(t *testing.T) {
	// The board was fetched before this flight left: scheduled 15:10, no
	// actual. We first saw it airborne at 15:45; allowing ~10 min from gate
	// to wheels-up puts the departure at 15:35, so 25 min late.
	r := Row{FlightICAO: "RYR1AB", AirlineIATA: "FR", DepIATA: "AGP", ArrIATA: "STN", DepSched: at(15, 10), Status: "scheduled"}
	st := StatusOf(r, false, at(15, 45))
	if st.Kind != flightstatus.KindLate || st.DelayMin != 25 || st.Basis != "departure" {
		t.Errorf("status = %+v", st)
	}
	// Seen 12 minutes after schedule → estimate 2 min late → on time.
	if st := StatusOf(r, false, at(15, 22)); st.Kind != flightstatus.KindOnTime || st.DelayMin != 2 {
		t.Errorf("status = %+v", st)
	}
	// The board's recorded actual always wins over our estimate.
	r.DepActual, r.DepEst = at(15, 12), at(15, 12)
	two := 2
	r.DepDelayMin = &two
	if st := StatusOf(r, false, at(16, 30)); st.Kind != flightstatus.KindOnTime || st.DelayMin != 2 {
		t.Errorf("actual must win: %+v", st)
	}
	// Without a sighting the old rule applies (estimated minus scheduled).
	r.DepActual, r.DepDelayMin = time.Time{}, nil
	r.DepEst = at(15, 20)
	if st := StatusOf(r, false, time.Time{}); st.Kind != flightstatus.KindLate || st.DelayMin != 10 {
		t.Errorf("no sighting: %+v", st)
	}
}

func TestMatch_DelayedDepartureWithoutActualStillMatches(t *testing.T) {
	// Scheduled 14:00, badly delayed, the board (fetched earlier) has no
	// actual yet. Seen airborne at 15:30: 90 min past schedule must still
	// match, since that is exactly the late flight we want to report.
	rows := []Row{{FlightICAO: "RYR9ZZ", AirlineIATA: "FR", DepIATA: "AGP", ArrIATA: "STN", DepSched: at(14, 0), Status: "scheduled"}}
	q := flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "STN", Seen: at(15, 30)}
	if r, ok := Match(rows, "AGP", q, at(15, 30)); !ok || r.FlightICAO != "RYR9ZZ" {
		t.Errorf("late departure must match: ok=%v", ok)
	}
	// But a row with a recorded actual keeps the tight 45 min window.
	rows[0].DepActual = at(14, 5)
	if _, ok := Match(rows, "AGP", q, at(15, 30)); ok {
		t.Error("an actual 85 min ago is not the aircraft seen now")
	}
}

func TestMatch_StaleFutureEstimateDoesNotBlockADelayedDeparture(t *testing.T) {
	// Scheduled 14:00, the board's (stale) estimate says 16:00, no actual.
	// The aircraft is airborne at 15:20: the schedule is what it is late
	// against, and the estimate must not veto the match.
	rows := []Row{{FlightICAO: "RYR9ZZ", AirlineIATA: "FR", DepIATA: "AGP", ArrIATA: "STN", DepSched: at(14, 0), DepEst: at(16, 0), Status: "scheduled"}}
	q := flightstatus.Query{Callsign: "RYR85UA", AirlineIATA: "FR", Origin: "AGP", Dest: "STN", Seen: at(15, 20)}
	r, ok := Match(rows, "AGP", q, at(15, 20))
	if !ok {
		t.Fatal("must match on the schedule")
	}
	if st := StatusOf(r, false, q.Seen); st.Kind != flightstatus.KindLate || st.DelayMin != 70 {
		t.Errorf("status = %+v", st)
	}
}
