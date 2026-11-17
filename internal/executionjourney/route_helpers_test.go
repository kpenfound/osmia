package executionjourney

import (
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// workTransitions returns the transitions of the workstream's work, in the order
// they were recorded: the router's own journal is bookkeeping, not a state
// change.
func workTransitions(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	all, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	return slices.DeleteFunc(all, func(tr trace.Transition) bool { return router.IsJournal(tr.Subject) })
}
