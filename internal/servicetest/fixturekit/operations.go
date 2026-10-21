package fixturekit

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func ActionOperation(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	Must(t, err)
	var found coreadapter.Operation
	var at time.Time
	for _, op := range ops {
		if op.Operation.Action == action && op.Transition.At.After(at) {
			found = op.Operation
			at = op.Transition.At
		}
	}
	if found.ID == "" {
		t.Fatalf("no %s operation", action)
	}
	return found
}
