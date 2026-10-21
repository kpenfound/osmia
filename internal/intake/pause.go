package intake

import (
	"errors"
	"strings"

	"github.com/kpenfound/osmia/internal/trace"
)

// errPaused leaves a reconciler operation pending instead of starting a turn:
// the controller holds the operation until the pause is lifted.
var ErrPaused = errors.New("a pause holds the workstream")

// continueMark separates the turn a pause continuation continues from the
// sequence of the stopped turn in the continuation's ID.
const ContinueMark = "-continue-"

// continuedTurn returns the turn a pause continuation continues, and any
// other turn itself.
func ContinuedTurn(turn string) string {
	base, _, _ := strings.Cut(turn, ContinueMark)
	return base
}

// isContinuation reports whether the turn continues a turn a hard pause
// stopped.
func IsContinuation(turn string) bool { return strings.Contains(turn, ContinueMark) }

// stoppedTurn reports whether a hard pause stopped the completed turn.
func StoppedTurn(q *trace.QueuedTurn) bool {
	return q != nil && !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Stop != nil
}
