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
	"sync"
	"time"
)

const DefaultHexDBURL = "https://hexdb.io"

// hexdbMaxAge bounds how old a route record may be. hexdb keeps the last
// route it saw for a callsign indefinitely, and airlines reuse callsigns
// for different city pairs, so an old record is as likely wrong as right.
const hexdbMaxAge = 30 * 24 * time.Hour

type HexDB struct {
	client    *http.Client
	baseURL   string
	userAgent string
	now       func() time.Time

	// Airports never move, so each ICAO is fetched once per process. Only
	// successes and 404s are stored: a transient failure must be retried.
	mu       sync.Mutex
	airports map[string]hexdbAirport
}

func NewHexDB(client *http.Client, baseURL, userAgent string, now func() time.Time) *HexDB {
	return &HexDB{client: client, baseURL: baseURL, userAgent: userAgent, now: now,
		airports: map[string]hexdbAirport{}}
}

type hexdbRouteResp struct {
	Route      string `json:"route"`
	UpdateTime int64  `json:"updatetime"`
}

type hexdbAirport struct {
	IATA    string   `json:"iata"`
	ICAO    string   `json:"icao"`
	Lat     *float64 `json:"latitude"`
	Lon     *float64 `json:"longitude"`
	missing bool     // hexdb answered 404 for this ICAO
}

var icaoRe = regexp.MustCompile(`^[A-Z]{4}$`)

// Route resolves a callsign to two airports with coordinates. hexdb has no
// airline or city names, so those stay empty. Anything it cannot vouch for
// (stale, multi-leg, unresolvable airport) is "unknown" rather than a guess.
func (h *HexDB) Route(ctx context.Context, callsign string) (Route, bool, error) {
	body, found, err := h.get(ctx, "/api/v1/route/icao/"+url.PathEscape(callsign))
	if err != nil || !found {
		return Route{}, false, err
	}
	var rr hexdbRouteResp
	if err := json.Unmarshal(body, &rr); err != nil {
		return Route{}, false, fmt.Errorf("hexdb: decode route: %w", err)
	}
	// A missing updatetime decodes to 0 and is treated as stale.
	if rr.UpdateTime <= 0 || h.now().Sub(time.Unix(rr.UpdateTime, 0)) > hexdbMaxAge {
		return Route{}, false, nil
	}
	// Exactly two legs: a three-airport string is a multi-stop service
	// where we cannot tell which leg the aircraft is flying.
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(rr.Route)), "-")
	if len(parts) != 2 || !icaoRe.MatchString(parts[0]) || !icaoRe.MatchString(parts[1]) {
		return Route{}, false, nil
	}
	o, err := h.airport(ctx, parts[0])
	if err != nil || o.missing {
		return Route{}, false, err
	}
	d, err := h.airport(ctx, parts[1])
	if err != nil || d.missing {
		return Route{}, false, err
	}
	if o.Lat == nil || o.Lon == nil || d.Lat == nil || d.Lon == nil {
		return Route{}, false, nil
	}
	return Route{
		OriginIATA: o.IATA, OriginICAO: parts[0],
		DestIATA: d.IATA, DestICAO: parts[1],
		HasCoords: true,
		OriginLat: *o.Lat, OriginLon: *o.Lon,
		DestLat: *d.Lat, DestLon: *d.Lon,
	}, true, nil
}

func (h *HexDB) airport(ctx context.Context, icao string) (hexdbAirport, error) {
	h.mu.Lock()
	a, ok := h.airports[icao]
	h.mu.Unlock()
	if ok {
		return a, nil
	}
	body, found, err := h.get(ctx, "/api/v1/airport/icao/"+url.PathEscape(icao))
	if err != nil {
		return hexdbAirport{}, err
	}
	if !found {
		a = hexdbAirport{missing: true}
	} else if err := json.Unmarshal(body, &a); err != nil {
		return hexdbAirport{}, fmt.Errorf("hexdb: decode airport: %w", err)
	}
	h.mu.Lock()
	h.airports[icao] = a
	h.mu.Unlock()
	return a, nil
}

// get returns found=false for 404, which hexdb uses for "never seen"; that
// is a stable unknown, not an outage.
func (h *HexDB) get(ctx context.Context, path string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", h.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("hexdb: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("hexdb: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, false, fmt.Errorf("hexdb: %w", err)
	}
	return body, true, nil
}
