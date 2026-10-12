// Package logos fetches airline logos by IATA code from a public CDN and
// caches them so one household costs the CDN one request per airline.
package logos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://pics.avs.io"
	hitTTL         = 7 * 24 * time.Hour
	missTTL        = time.Hour
	maxBody        = 64 << 10
	maxEntries     = 256
	fetchTimeout   = 8 * time.Second
)

var codeRe = regexp.MustCompile(`^[A-Z0-9]{2}$`)

// ValidCode reports whether s is an upper-case two-character IATA
// airline code; it is the only shape that ever reaches the CDN URL.
func ValidCode(s string) bool { return codeRe.MatchString(s) }

type entry struct {
	png     []byte
	expires time.Time
}

type Source struct {
	client    *http.Client
	baseURL   string
	userAgent string
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

func New(client *http.Client, baseURL, userAgent string, now func() time.Time) *Source {
	return &Source{client: client, baseURL: strings.TrimRight(baseURL, "/"), userAgent: userAgent, now: now,
		entries: map[string]entry{}}
}

// Get returns the PNG for an airline code. found is false for a code the
// CDN does not know or that failed validation; err is set only on the
// first failed fetch, after which the miss is served from cache for an
// hour so a 3-second poll loop cannot hammer an unreachable CDN.
func (s *Source) Get(ctx context.Context, iata string) ([]byte, bool, error) {
	if !ValidCode(iata) {
		return nil, false, nil
	}
	now := s.now()
	s.mu.Lock()
	if e, ok := s.entries[iata]; ok && now.Before(e.expires) {
		s.mu.Unlock()
		return e.png, e.png != nil, nil
	}
	s.mu.Unlock()

	// Detached from the caller: a browser tab closing or the UI proxy's
	// 5-second timeout must not turn into a cached one-hour miss for the
	// whole household. The fetch gets its own bound instead.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	png, err := s.fetch(fctx, iata)
	ttl := missTTL
	if png != nil {
		ttl = hitTTL
	}
	s.mu.Lock()
	if len(s.entries) >= maxEntries {
		s.entries = map[string]entry{}
	}
	s.entries[iata] = entry{png: png, expires: now.Add(ttl)}
	s.mu.Unlock()
	return png, png != nil, err
}

// fetch returns nil bytes for every "no logo" outcome; err is non-nil only
// for transport failures, so callers can log an outage once.
func (s *Source) fetch(ctx context.Context, iata string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/240/80/"+iata+".png", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("logos: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/png") {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("logos: %w", err)
	}
	if len(body) > maxBody || len(body) == 0 {
		return nil, nil
	}
	return body, nil
}
