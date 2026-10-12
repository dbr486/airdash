// Package geocode turns a typed address into coordinates through
// OpenStreetMap's Nominatim, so the home position can be set from the
// settings page. Nominatim's usage policy allows one request per second
// with an identifying User-Agent; both are enforced here, and answers are
// cached because the same address gets typed again.
package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultURL = "https://nominatim.openstreetmap.org"
	minQuery   = 3
	cacheTTL   = 24 * time.Hour
	minGap     = time.Second
)

var ErrThrottled = errors.New("geocode: one lookup per second")

type Result struct {
	Lat, Lon float64
	Name     string
}

type Lookup interface {
	Lookup(ctx context.Context, q string) (Result, bool, error)
}

type Nominatim struct {
	client    *http.Client
	baseURL   string
	userAgent string
	now       func() time.Time

	mu      sync.Mutex
	last    time.Time
	entries map[string]entry
}

type entry struct {
	res     Result
	ok      bool
	expires time.Time
}

func New(client *http.Client, baseURL, userAgent string, now func() time.Time) *Nominatim {
	return &Nominatim{client: client, baseURL: strings.TrimRight(baseURL, "/"), userAgent: userAgent, now: now, entries: map[string]entry{}}
}

// Lookup returns the best match for q. found is false when Nominatim knows
// nothing; ErrThrottled when a different query arrives within a second of
// the previous upstream call (cache hits are never throttled).
func (n *Nominatim) Lookup(ctx context.Context, q string) (Result, bool, error) {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) < minQuery {
		return Result{}, false, fmt.Errorf("geocode: query too short")
	}
	key := strings.ToLower(q)
	now := n.now()
	n.mu.Lock()
	if e, ok := n.entries[key]; ok && now.Before(e.expires) {
		n.mu.Unlock()
		return e.res, e.ok, nil
	}
	if now.Sub(n.last) < minGap {
		n.mu.Unlock()
		return Result{}, false, ErrThrottled
	}
	n.last = now
	n.mu.Unlock()

	res, ok, err := n.fetch(ctx, q)
	if err != nil {
		return Result{}, false, err
	}
	n.mu.Lock()
	n.entries[key] = entry{res: res, ok: ok, expires: now.Add(cacheTTL)}
	n.mu.Unlock()
	return res, ok, nil
}

func (n *Nominatim) fetch(ctx context.Context, q string) (Result, bool, error) {
	v := url.Values{"q": {q}, "format": {"jsonv2"}, "limit": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.baseURL+"/search?"+v.Encode(), nil)
	if err != nil {
		return Result{}, false, err
	}
	req.Header.Set("User-Agent", n.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Result{}, false, fmt.Errorf("geocode: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, false, fmt.Errorf("geocode: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return Result{}, false, fmt.Errorf("geocode: %w", err)
	}
	var rows []struct {
		Lat  string `json:"lat"`
		Lon  string `json:"lon"`
		Name string `json:"display_name"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return Result{}, false, fmt.Errorf("geocode: decode: %w", err)
	}
	if len(rows) == 0 {
		return Result{}, false, nil
	}
	lat, err1 := strconv.ParseFloat(rows[0].Lat, 64)
	lon, err2 := strconv.ParseFloat(rows[0].Lon, 64)
	if err1 != nil || err2 != nil {
		return Result{}, false, fmt.Errorf("geocode: bad coordinates in response")
	}
	return Result{Lat: lat, Lon: lon, Name: rows[0].Name}, true, nil
}
