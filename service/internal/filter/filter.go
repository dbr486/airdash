// Package filter applies the optional airport rules from the config.
// Within one variable the codes are OR-ed, across variables AND-ed. The
// tracker only calls Match for routes that are known; an unknown route
// under an active filter is excluded by the tracker before it gets here.
package filter

import (
	"fmt"
	"strings"

	"airdash/internal/routes"
)

type Filter struct {
	origin  map[string]bool
	dest    map[string]bool
	airport map[string]bool
}

func Parse(origin, dest, airport string) (Filter, error) {
	var f Filter
	var err error
	if f.origin, err = parseList("AIRDASH_ORIGIN", origin); err != nil {
		return Filter{}, err
	}
	if f.dest, err = parseList("AIRDASH_DEST", dest); err != nil {
		return Filter{}, err
	}
	if f.airport, err = parseList("AIRDASH_AIRPORT", airport); err != nil {
		return Filter{}, err
	}
	return f, nil
}

func parseList(name, raw string) (map[string]bool, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	set := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		code := strings.ToUpper(strings.TrimSpace(part))
		if !validCode(code) {
			return nil, fmt.Errorf("%s: %q is not a 3-letter IATA or 4-letter ICAO code", name, part)
		}
		set[code] = true
	}
	return set, nil
}

func validCode(code string) bool {
	if len(code) != 3 && len(code) != 4 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func (f Filter) Active() bool {
	return len(f.origin) > 0 || len(f.dest) > 0 || len(f.airport) > 0
}

func (f Filter) Match(r routes.Route) bool {
	if len(f.origin) > 0 && !either(f.origin, r.OriginIATA, r.OriginICAO) {
		return false
	}
	if len(f.dest) > 0 && !either(f.dest, r.DestIATA, r.DestICAO) {
		return false
	}
	if len(f.airport) > 0 &&
		!either(f.airport, r.OriginIATA, r.OriginICAO) &&
		!either(f.airport, r.DestIATA, r.DestICAO) {
		return false
	}
	return true
}

func either(set map[string]bool, iata, icao string) bool {
	return (iata != "" && set[iata]) || (icao != "" && set[icao])
}
