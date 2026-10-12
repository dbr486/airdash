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
	Status          string  `json:"status"`
	ProgressPercent *int    `json:"progress_percent"`
	ActualOff       *string `json:"actual_off"`
	ActualOn        *string `json:"actual_on"`
	DepartureDelay  *int    `json:"departure_delay"`
	ArrivalDelay    *int    `json:"arrival_delay"`
	Cancelled       bool    `json:"cancelled"`
	Diverted        bool    `json:"diverted"`
}

type flightsResponse struct {
	Flights []flight `json:"flights"`
}

func (a *AeroAPI) Status(ctx context.Context, q Query) (Status, bool, error) {
	u := a.baseURL + "/flights/" + url.PathEscape(q.Callsign) + "?max_pages=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Status{}, false, err
	}
	req.Header.Set("x-apikey", a.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", a.userAgent)
	resp, err := a.client.Do(req)
	if err != nil {
		return Status{}, false, fmt.Errorf("aeroapi: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound:
		return Status{}, false, nil // ident FlightAware cannot resolve: stable unknown
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Status{}, false, fmt.Errorf("%w (status %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Status{}, false, fmt.Errorf("aeroapi: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Status{}, false, fmt.Errorf("aeroapi: %w", err)
	}
	var fr flightsResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return Status{}, false, fmt.Errorf("aeroapi: decode: %w", err)
	}
	f, ok := pickFlight(fr.Flights)
	if !ok {
		return Status{}, false, nil
	}
	st, ok := classify(f)
	return st, ok, nil
}

// pickFlight chooses the instance that is in the air now; AeroAPI returns
// several days of the same flight number.
func pickFlight(fs []flight) (flight, bool) {
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
	// No airborne instance: the caller only ever looks up an aircraft that
	// is currently in the air, so anything left (e.g. a cancelled or
	// diverted instance from an earlier day, or one not yet departed) is
	// not the flight actually overhead. Report unknown rather than
	// guessing wrong and caching it for 15 minutes.
	return flight{}, false
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
