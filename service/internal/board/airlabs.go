package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultAirLabsURL = "https://airlabs.co/api/v9"

// AirLabs fetches the schedules endpoint. The key rides in the query
// string as the API requires; it is never logged.
type AirLabs struct {
	client    *http.Client
	baseURL   string
	key       string
	userAgent string
}

func NewAirLabs(client *http.Client, baseURL, key, userAgent string) *AirLabs {
	return &AirLabs{client: client, baseURL: strings.TrimRight(baseURL, "/"), key: key, userAgent: userAgent}
}

// Fetch returns one direction of the board. ICAO codes (4 letters) use the
// *_icao parameter, IATA codes (3) the *_iata one.
func (a *AirLabs) Fetch(ctx context.Context, airport string, arrivals bool) ([]Row, error) {
	dir := "dep"
	if arrivals {
		dir = "arr"
	}
	kind := "iata"
	if len(airport) == 4 {
		kind = "icao"
	}
	q := url.Values{dir + "_" + kind: {airport}, "limit": {"500"}, "api_key": {a.key}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/schedules?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// url.Error prints the full URL, key included; keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("airlabs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("airlabs: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("airlabs: %w", err)
	}
	return ParseAirLabs(body)
}

type airlabsEnvelope struct {
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
	Response []airlabsRow `json:"response"`
}

type airlabsRow struct {
	FlightIATA  *string `json:"flight_iata"`
	FlightICAO  *string `json:"flight_icao"`
	AirlineIATA *string `json:"airline_iata"`
	DepIATA     *string `json:"dep_iata"`
	DepICAO     *string `json:"dep_icao"`
	ArrIATA     *string `json:"arr_iata"`
	ArrICAO     *string `json:"arr_icao"`
	DepTime     *string `json:"dep_time_utc"`
	DepEst      *string `json:"dep_estimated_utc"`
	DepActual   *string `json:"dep_actual_utc"`
	ArrTime     *string `json:"arr_time_utc"`
	ArrEst      *string `json:"arr_estimated_utc"`
	ArrActual   *string `json:"arr_actual_utc"`
	DepDelayed  *int    `json:"dep_delayed"`
	ArrDelayed  *int    `json:"arr_delayed"`
	Status      *string `json:"status"`
}

func ParseAirLabs(body []byte) ([]Row, error) {
	var env airlabsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("airlabs: decode: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("airlabs: %s (%s)", env.Error.Message, env.Error.Code)
	}
	rows := make([]Row, 0, len(env.Response))
	for _, r := range env.Response {
		rows = append(rows, Row{
			FlightIATA: str(r.FlightIATA), FlightICAO: strings.ToUpper(str(r.FlightICAO)), AirlineIATA: strings.ToUpper(str(r.AirlineIATA)),
			DepIATA: str(r.DepIATA), DepICAO: str(r.DepICAO), ArrIATA: str(r.ArrIATA), ArrICAO: str(r.ArrICAO),
			DepSched: stamp(r.DepTime), DepEst: stamp(r.DepEst), DepActual: stamp(r.DepActual),
			ArrSched: stamp(r.ArrTime), ArrEst: stamp(r.ArrEst), ArrActual: stamp(r.ArrActual),
			DepDelayMin: r.DepDelayed, ArrDelayMin: r.ArrDelayed, Status: str(r.Status),
		})
	}
	return rows, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// stamp parses the board's "2006-01-02 15:04" UTC strings; anything else
// is treated as absent rather than failing the whole board.
func stamp(p *string) time.Time {
	if p == nil {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", *p, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}
