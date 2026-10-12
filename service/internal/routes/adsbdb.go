package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const DefaultADSBDBURL = "https://api.adsbdb.com"

type ADSBDB struct {
	client    *http.Client
	baseURL   string
	userAgent string
}

func NewADSBDB(client *http.Client, baseURL, userAgent string) *ADSBDB {
	return &ADSBDB{client: client, baseURL: baseURL, userAgent: userAgent}
}

// adsbdb answers either {"response":{"flightroute":{...}}} or
// {"response":"unknown callsign"}, so "response" is decoded lazily.
type adsbdbEnvelope struct {
	Response json.RawMessage `json:"response"`
}

type adsbdbRoute struct {
	FlightRoute struct {
		Airline struct {
			Name string `json:"name"`
			IATA string `json:"iata"`
		} `json:"airline"`
		Origin      adsbdbAirport `json:"origin"`
		Destination adsbdbAirport `json:"destination"`
	} `json:"flightroute"`
}

type adsbdbAirport struct {
	IATA         string `json:"iata_code"`
	ICAO         string `json:"icao_code"`
	Municipality string `json:"municipality"`
}

func (a *ADSBDB) Route(ctx context.Context, callsign string) (Route, bool, error) {
	u := a.baseURL + "/v0/callsign/" + url.PathEscape(callsign)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Route{}, false, err
	}
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return Route{}, false, fmt.Errorf("adsbdb: %w", err)
	}
	defer resp.Body.Close()

	// adsbdb uses 404 for callsigns it has never seen; that is a stable
	// "unknown", not an outage.
	if resp.StatusCode == http.StatusNotFound {
		return Route{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Route{}, false, fmt.Errorf("adsbdb: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Route{}, false, fmt.Errorf("adsbdb: %w", err)
	}
	return decodeADSBDB(body)
}

// Only a well-formed two-character IATA code is kept: it is the logo key
// and part of the fixed-size JSON the display parses, so an odd value
// from adsbdb must not grow that body.
var airlineIATARe = regexp.MustCompile(`^[A-Z0-9]{2}$`)

func airlineIATA(raw string) string {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if !airlineIATARe.MatchString(code) {
		return ""
	}
	return code
}

func decodeADSBDB(body []byte) (Route, bool, error) {
	var env adsbdbEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Route{}, false, fmt.Errorf("adsbdb: decode: %w", err)
	}
	var asString string
	if json.Unmarshal(env.Response, &asString) == nil {
		// e.g. "unknown callsign"
		return Route{}, false, nil
	}
	var r adsbdbRoute
	if err := json.Unmarshal(env.Response, &r); err != nil {
		return Route{}, false, fmt.Errorf("adsbdb: decode route: %w", err)
	}
	fr := r.FlightRoute
	if fr.Origin.IATA == "" && fr.Origin.ICAO == "" {
		return Route{}, false, nil
	}
	return Route{
		Airline:     fr.Airline.Name,
		AirlineIATA: airlineIATA(fr.Airline.IATA),
		OriginIATA:  fr.Origin.IATA,
		OriginICAO:  fr.Origin.ICAO,
		DestIATA:    fr.Destination.IATA,
		DestICAO:    fr.Destination.ICAO,
		OriginName:  asciiFold(fr.Origin.Municipality),
		DestName:    asciiFold(fr.Destination.Municipality),
	}, true, nil
}
