// Package provider fetches aircraft near a point from a community ADS-B
// aggregator. adsb.lol and adsb.fi both speak the readsb JSON dialect and
// differ only in URL layout and the name of the list key.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ErrRateLimited marks a fetch error caused by an HTTP 429 from the
// upstream feed, so the tracker can back off immediately instead of
// waiting for the normal consecutive-failure threshold.
var ErrRateLimited = errors.New("provider: rate limited")

type Aircraft struct {
	Hex      string
	Callsign string
	Type     string
	Reg      string
	Lat, Lon float64
	HasPos   bool
	AltFt    int
	OnGround bool
	GroundKt int
	Track    int
}

type Provider interface {
	Fetch(ctx context.Context, lat, lon, radiusKm float64) ([]Aircraft, error)
}

const (
	DefaultADSBLolURL = "https://api.adsb.lol"
	DefaultADSBFiURL  = "https://opendata.adsb.fi"
)

func New(name string, client *http.Client, userAgent string) (Provider, error) {
	switch name {
	case "adsblol":
		return NewADSBLol(client, DefaultADSBLolURL, userAgent), nil
	case "adsbfi":
		return NewADSBFi(client, DefaultADSBFiURL, userAgent), nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}
