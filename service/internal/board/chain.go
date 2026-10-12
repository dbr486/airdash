package board

import (
	"context"

	"airdash/internal/flightstatus"
)

type chain struct{ first, second flightstatus.Lookup }

// Chain asks first and only falls through to second on a miss, so a free
// board hit never spends a paid lookup. A nil second just returns misses.
func Chain(first, second flightstatus.Lookup) flightstatus.Lookup { return chain{first, second} }

func (c chain) Status(ctx context.Context, q flightstatus.Query) (flightstatus.Status, bool, error) {
	st, ok, err := c.first.Status(ctx, q)
	if ok || c.second == nil {
		return st, ok, err
	}
	return c.second.Status(ctx, q)
}
