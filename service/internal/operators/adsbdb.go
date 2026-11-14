// Package operators names the airline operating an aircraft when the live
// feed and route lookup cannot: first from the callsign's ICAO prefix, then
// from the registered owner recorded by adsbdb.
package operators

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// FlagLookup returns the registered owner/operator's ICAO airline code
// ("flag code") for an aircraft's ICAO24 hex.
type FlagLookup interface {
	FlagCode(ctx context.Context, hex string) (code string, ok bool, err error)
}

type ADSBDBOwner struct {
	client    *http.Client
	baseURL   string
	userAgent string
}

func NewADSBDBOwner(client *http.Client, baseURL, userAgent string) *ADSBDBOwner {
	return &ADSBDBOwner{client: client, baseURL: baseURL, userAgent: userAgent}
}

// adsbdb answers either {"response":{"aircraft":{...}}} or
// {"response":"unknown aircraft"}, so "response" is decoded lazily.
type aircraftEnvelope struct {
	Response json.RawMessage `json:"response"`
}

type aircraftBody struct {
	Aircraft struct {
		FlagCode string `json:"registered_owner_operator_flag_code"`
	} `json:"aircraft"`
}

func (a *ADSBDBOwner) FlagCode(ctx context.Context, hex string) (string, bool, error) {
	u := a.baseURL + "/v0/aircraft/" + url.PathEscape(hex)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("adsbdb aircraft: %w", err)
	}
	defer resp.Body.Close()

	// 404 means adsbdb has never seen this airframe: a stable "unknown",
	// not an outage.
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("adsbdb aircraft: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", false, fmt.Errorf("adsbdb aircraft: %w", err)
	}
	var env aircraftEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", false, fmt.Errorf("adsbdb aircraft: decode: %w", err)
	}
	var asString string
	if json.Unmarshal(env.Response, &asString) == nil {
		return "", false, nil
	}
	var b aircraftBody
	if err := json.Unmarshal(env.Response, &b); err != nil {
		return "", false, fmt.Errorf("adsbdb aircraft: decode aircraft: %w", err)
	}
	if b.Aircraft.FlagCode == "" {
		return "", false, nil
	}
	return b.Aircraft.FlagCode, true, nil
}
