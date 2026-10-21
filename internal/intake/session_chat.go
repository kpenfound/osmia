package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
)

// unfinishedSessionTurn reports whether the thread holds a turn with no
// response and no completion: one still queued, claimed or captured
// without being completed.
func UnfinishedSessionTurn(th trace.Thread) bool {
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() {
			return true
		}
	}
	return false
}
