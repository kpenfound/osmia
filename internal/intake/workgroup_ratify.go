package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
)

// queuedTurn reports whether the thread already has a queued turn with the
// given ID.
func QueuedTurn(t trace.Thread, turn string) bool {
	for _, q := range t.Turns {
		if q.Request.TurnID == turn {
			return true
		}
	}
	return false
}
