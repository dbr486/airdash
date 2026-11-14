package flightstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultAeroAPIURL = "https://aeroapi.flightaware.com/aeroapi"

// ErrAuth marks a 401/403 from AeroAPI: the key is missing or revoked, so
// retrying soon (the normal error cooldown) cannot succeed and would only
// burn the daily budget on the same failure.
var ErrAuth = errors.New("aeroapi: authentication failed")

type AeroAPI struct {
	client    *http.Client
	baseURL   string
	apiKey    string
	userAgent string
}

func NewAeroAPI(client *http.Client, baseURL, apiKey, userAgent string) *AeroAPI {
	return &AeroAPI{client: client, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, userAgent: userAgent}
}

// flight is the subset of an AeroAPI flight object we read. Pointers
// distinguish null from zero, which matters for the delay fields.
type flight struct {
	Ident           string  `json:"ident"`
	Registration    string  `json:"registration"`
	Status          string  `json:"status"`
	ProgressPercent *int    `json:"progress_percent"`
	ScheduledOff    *string `json:"scheduled_off"`
	EstimatedOff    *string `json:"estimated_off"`
	ActualOff       *string `json:"actual_off"`
	ActualOn        *string `json:"actual_on"`
	DepartureDelay  *int    `json:"departure_delay"`
	ArrivalDelay    *int    `json:"arrival_delay"`
	Cancelled       bool    `json:"cancelled"`
	Diverted        bool    `json:"diverted"`
	Operator        string  `json:"operator"`
	OperatorIATA    string  `json:"operator_iata"`
	Origin          *place  `json:"origin"`
	Destination     *place  `json:"destination"`
}

// place is an AeroAPI airport reference; the object is null for flights
// FlightAware has no endpoint for.
type place struct {
	CodeIATA string `json:"code_iata"`
	CodeICAO string `json:"code_icao"`
	City     string `json:"city"`
}

type flightsResponse struct {
	Flights []flight `json:"flights"`
}

func (a *AeroAPI) Status(ctx context.Context, q Query) (Status, bool, error) {
	f, ok, err := a.Flight(ctx, q)
	return statusOf(f, ok, err)
}

func (a *AeroAPI) Flight(ctx context.Context, q Query) (Flight, bool, error) {
	u := a.baseURL + "/flights/" + url.PathEscape(q.Callsign) + "?max_pages=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Flight{}, false, err
	}
	req.Header.Set("x-apikey", a.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", a.userAgent)
	resp, err := a.client.Do(req)
	if err != nil {
		return Flight{}, false, fmt.Errorf("aeroapi: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound:
		return Flight{}, false, nil // ident FlightAware cannot resolve: stable unknown
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Flight{}, false, fmt.Errorf("%w (status %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Flight{}, false, fmt.Errorf("aeroapi: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Flight{}, false, fmt.Errorf("aeroapi: %w", err)
	}
	var fr flightsResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return Flight{}, false, fmt.Errorf("aeroapi: decode: %w", err)
	}
	f, ok := pickFlight(fr.Flights, q.Seen, q.Reg)
	if !ok {
		return Flight{}, false, nil
	}
	out := Flight{Route: routeOf(f)}
	out.HasRoute = out.Route.OriginIATA != "" || out.Route.OriginICAO != "" ||
		out.Route.DestIATA != "" || out.Route.DestICAO != ""
	out.Status, out.HasStatus = classify(f)
	return out, out.HasRoute || out.HasStatus, nil
}

// These bound how far an instance's expected take-off may sit from the
// aircraft's first sighting in range: FlightAware's estimate can trail a
// delayed departure, and a just-departed aircraft is sighted minutes after.
// The short after-bound keeps out an inbound aircraft's next leg, which
// cannot leave within minutes of the aircraft being seen on approach.
const (
	takeoffBeforeSeen = 60 * time.Minute
	takeoffAfterSeen  = 10 * time.Minute
)

// pickFlight chooses the instance that is in the air now; AeroAPI returns
// several days of the same flight number. seen is when the aircraft was
// first sighted airborne in range and reg its ADS-B registration (either
// may be empty).
func pickFlight(fs []flight, seen time.Time, reg string) (flight, bool) {
	for _, f := range fs {
		if f.ActualOff != nil && f.ActualOn == nil {
			return f, true
		}
	}
	for _, f := range fs {
		if strings.HasPrefix(f.Status, "En Route") {
			return f, true
		}
	}
	for _, f := range fs {
		if f.ProgressPercent != nil && *f.ProgressPercent >= 1 && *f.ProgressPercent <= 99 {
			return f, true
		}
	}
	// FlightAware logs a take-off a minute or two late, so an aircraft just
	// airborne can still be "Scheduled" there. The caller only looks up
	// aircraft already in the air, so the one instance due off around the
	// first sighting and assigned this very tail is the flight overhead;
	// two of them fail closed.
	if f, ok := departingAt(fs, seen, reg); ok {
		return f, true
	}
	// Anything left (a cancelled or diverted instance from an earlier day,
	// a later leg) is not the flight actually overhead. Report unknown
	// rather than guessing wrong and caching it for 15 minutes.
	return flight{}, false
}

func departingAt(fs []flight, seen time.Time, reg string) (flight, bool) {
	tail := normReg(reg)
	if seen.IsZero() || tail == "" {
		return flight{}, false
	}
	var hit flight
	n := 0
	for _, f := range fs {
		if f.Cancelled || f.ActualOn != nil || normReg(f.Registration) != tail {
			continue
		}
		off, ok := parseTime(f.EstimatedOff)
		if !ok {
			off, ok = parseTime(f.ScheduledOff)
		}
		if !ok {
			continue
		}
		if d := seen.Sub(off); d <= takeoffBeforeSeen && d >= -takeoffAfterSeen {
			hit = f
			n++
		}
	}
	return hit, n == 1
}

func normReg(r string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(r), "-", ""))
}

func parseTime(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, *s)
	return t, err == nil
}

func classify(f flight) (Status, bool) {
	if f.Cancelled {
		return Status{Kind: KindCancelled}, true
	}
	if f.Diverted {
		return Status{Kind: KindDiverted}, true
	}
	var secs int
	var basis string
	switch {
	case f.ArrivalDelay != nil:
		secs, basis = *f.ArrivalDelay, "arrival"
	case f.DepartureDelay != nil:
		secs, basis = *f.DepartureDelay, "departure"
	default:
		return Status{}, false
	}
	return Classify(int(math.Round(float64(secs)/60)), basis), true
}

func routeOf(f flight) FlightRoute {
	r := FlightRoute{Operator: f.Operator, OperatorIATA: f.OperatorIATA}
	if o := f.Origin; o != nil {
		r.OriginIATA, r.OriginICAO, r.OriginCity = o.CodeIATA, o.CodeICAO, asciiFold(o.City)
	}
	if d := f.Destination; d != nil {
		r.DestIATA, r.DestICAO, r.DestCity = d.CodeIATA, d.CodeICAO, asciiFold(d.City)
	}
	return r
}
