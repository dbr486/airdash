package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const kmPerNm = 1.852

type readsbProvider struct {
	client    *http.Client
	userAgent string
	url       func(lat, lon float64, radiusNm int) string
}

func NewADSBLol(client *http.Client, baseURL, userAgent string) Provider {
	return &readsbProvider{client: client, userAgent: userAgent,
		url: func(lat, lon float64, nm int) string {
			return fmt.Sprintf("%s/v2/point/%s/%s/%d", baseURL, fmtCoord(lat), fmtCoord(lon), nm)
		}}
}

func NewADSBFi(client *http.Client, baseURL, userAgent string) Provider {
	return &readsbProvider{client: client, userAgent: userAgent,
		url: func(lat, lon float64, nm int) string {
			return fmt.Sprintf("%s/api/v2/lat/%s/lon/%s/dist/%d", baseURL, fmtCoord(lat), fmtCoord(lon), nm)
		}}
}

func fmtCoord(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Upstream wants whole nautical miles; round up so nothing inside the km
// radius is missed. The tracker re-checks distance in km.
func radiusNm(km float64) int {
	nm := int(math.Ceil(km / kmPerNm))
	if nm < 1 {
		nm = 1
	}
	return nm
}

func (p *readsbProvider) Fetch(ctx context.Context, lat, lon, radiusKm float64) ([]Aircraft, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url(lat, lon, radiusNm(radiusKm)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w (status 429)", ErrRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	return decodeReadsb(body)
}

type readsbEnvelope struct {
	AC       []readsbAircraft `json:"ac"`       // adsb.lol
	Aircraft []readsbAircraft `json:"aircraft"` // adsb.fi
	Error    any              `json:"error"`    // an upstream error envelope
}

type readsbAircraft struct {
	Hex     string          `json:"hex"`
	Flight  string          `json:"flight"`
	Reg     string          `json:"r"`
	Type    string          `json:"t"`
	Cat     string          `json:"category"` // ADS-B emitter category, e.g. A1 light, A3 large
	AltBaro json.RawMessage `json:"alt_baro"` // number, or the string "ground"
	GS      float64         `json:"gs"`
	Track   float64         `json:"track"`
	Lat     *float64        `json:"lat"`
	Lon     *float64        `json:"lon"`
	NavQNH  *float64        `json:"nav_qnh"`
}

func decodeReadsb(body []byte) ([]Aircraft, error) {
	var env readsbEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrBadBody, err)
	}
	// Neither key present (e.g. an upstream error body like {"error":"..."})
	// is a decode failure, not a valid zero-aircraft result; an explicit
	// empty list ("ac":[]) is still fine and falls through below.
	if env.AC == nil && env.Aircraft == nil {
		if env.Error != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadBody, env.Error)
		}
		return nil, fmt.Errorf("%w: no aircraft list", ErrBadBody)
	}
	list := env.AC
	if list == nil {
		list = env.Aircraft
	}
	// Aircraft without their own nav_qnh borrow the area median, so gather
	// it before correcting anyone. Only airborne aircraft below the
	// transition altitude count: above it crews select standard 1013, which
	// would drag the median toward "no correction" for everyone below.
	var qnhs []float64
	for _, r := range list {
		if r.NavQNH == nil || !validQNH(*r.NavQNH) {
			continue
		}
		if alt, hasBaro, onGround := parseAltBaro(r.AltBaro); hasBaro && !onGround && alt < transitionAltFt {
			qnhs = append(qnhs, *r.NavQNH)
		}
	}
	areaQNH, haveArea := medianQNH(qnhs)

	out := make([]Aircraft, 0, len(list))
	for _, r := range list {
		a := Aircraft{
			Hex:      r.Hex,
			Callsign: strings.TrimSpace(r.Flight),
			Type:     r.Type,
			Reg:      r.Reg,
			Category: r.Cat,
			GroundKt: int(math.Round(r.GS)),
			Track:    ((int(math.Round(r.Track)) % 360) + 360) % 360,
		}
		if r.Lat != nil && r.Lon != nil {
			a.HasPos, a.Lat, a.Lon = true, *r.Lat, *r.Lon
		}
		// A missing or unparseable alt_baro leaves AltFt at 0, which is
		// "unknown", not sea level; correcting it would invent an altitude.
		alt, hasBaro, onGround := parseAltBaro(r.AltBaro)
		a.OnGround = onGround
		if hasBaro {
			a.AltFt = alt
		}
		if hasBaro && !a.OnGround && a.AltFt < transitionAltFt {
			if r.NavQNH != nil && validQNH(*r.NavQNH) {
				a.AltFt = correctQNH(a.AltFt, *r.NavQNH)
			} else if haveArea {
				a.AltFt = correctQNH(a.AltFt, areaQNH)
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// parseAltBaro decodes readsb's alt_baro, which is a number or the string
// "ground". hasBaro is false for ground, missing and unparseable values.
func parseAltBaro(raw json.RawMessage) (alt int, hasBaro, onGround bool) {
	b := bytes.TrimSpace(raw)
	switch {
	case bytes.Equal(b, []byte(`"ground"`)):
		return 0, false, true
	case len(b) > 0:
		if f, err := strconv.ParseFloat(string(b), 64); err == nil {
			return int(math.Round(f)), true, false
		}
	}
	return 0, false, false
}
