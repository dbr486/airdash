package operators

import (
	"context"
	"regexp"
	"strings"

	"airdash/internal/standing"
)

var icaoHexRe = regexp.MustCompile(`^[0-9a-f]{6}$`)

// Resolver turns a callsign/hex pair into a display airline.
type Resolver struct {
	owners FlagLookup
}

func NewResolver(owners FlagLookup) *Resolver { return &Resolver{owners: owners} }

// Operator only ever returns names from the embedded VRS airline list. The
// owner lookup yields just a flag code, which is mapped through that list;
// adsbdb's free-text owner name is never shown because it can be a lessor,
// a private individual or other noise rather than the operating airline.
func (r *Resolver) Operator(ctx context.Context, callsign, hex string, haveAirline bool) (standing.Airline, bool, error) {
	// The callsign prefix is the operating airline by definition, so it wins
	// over the registered owner (which may be a lessor).
	if prefix, ok := standing.CallsignPrefix(callsign); ok {
		if a, ok := standing.LookupAirline(prefix); ok {
			return a, true, nil
		}
	}
	// An airline already known upstream needs no extra call.
	if haveAirline {
		return standing.Airline{}, false, nil
	}
	// Non-ICAO addresses (e.g. "~" TIS-B/anonymised ids) have no registry entry.
	hex = strings.ToLower(hex)
	if !icaoHexRe.MatchString(hex) {
		return standing.Airline{}, false, nil
	}
	code, ok, err := r.owners.FlagCode(ctx, hex)
	if err != nil {
		return standing.Airline{}, false, err
	}
	if !ok {
		return standing.Airline{}, false, nil
	}
	a, ok := standing.LookupAirline(code)
	return a, ok, nil
}
