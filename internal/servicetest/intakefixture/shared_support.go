package intakefixture

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// RequestJSON returns the JSON a Jev request is sent as.
func RequestJSON(t *testing.T, r systemone.Request) string {
	t.Helper()
	data, err := json.Marshal(r)
	fixturekit.Must(t, err)
	return string(data)
}

// WorkTransitions returns the workstream's transitions other than the
// router's journal entries.
func WorkTransitions(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	all, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(all, func(tr trace.Transition) bool { return router.IsJournal(tr.Subject) })
}

// UnitWorkflow returns the workflow state of the unit in the stream.
func UnitWorkflow(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, unit string) trace.WorkflowState {
	t.Helper()
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	fixturekit.Must(t, err)
	return state
}
