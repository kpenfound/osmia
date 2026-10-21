package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// attemptOperation reconciles one pending operation as the reconciliation
// controller does and returns what its effect returned: an effect that
// fails records a retry and no result, so the operation stays pending.
func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

// awaitResolution attempts the drift operation and fails unless it waits
// for its conflict resolution.
func awaitResolution(t *testing.T, s *Service, d drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	if result, err := attemptOperation(t, s, d.repository, stream, op, d); !errors.Is(err, errDriftAwaits) || !strings.Contains(err.Error(), want) {
		t.Fatalf("the drift rebase did not await %q: %+v %v", want, result, err)
	}
}

// completeDriftTurn runs the oldest unfinished turn of the workstream's
// drift agent as a finished turn that ends with outcome, and returns it.
func completeDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return intakefixture.CompleteDriftTurn(t, f.sharedTurns(), repository, stream, agent, outcome)
}

// resolvedDone is the outcome of a drift mason turn whose done the service
// accepted.
func resolvedDone(outcome string) *coreadapter.Outcome {
	data, _ := json.Marshal(MasonReport{Outcome: outcome})
	return &coreadapter.Outcome{Status: masonDone, Report: string(data)}
}

// driftVerdict is the outcome of a drift reviewer turn that recorded
// verdict.
func driftVerdict(t *testing.T, verdict UnitVerdict) *coreadapter.Outcome {
	t.Helper()
	return intakefixture.VerdictOutcome(t, verdict, verdictOutcome)
}

var (
	approvedResolution = UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
)

// turnIDs returns the turn IDs of the workstream's agent thread, or none
// when it has no thread.
func turnIDs(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent string) []string {
	t.Helper()
	return intakefixture.TurnIDs(t, repository, stream, agent)
}

// resolutionWorkspace returns the workstream's drift resolution workspace.
func resolutionWorkspace(t *testing.T, f *shedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	return intakefixture.Workspace(t, workspaces(f.s.cfg, driftsDirectory, backendOf(t, f, stream)), stream)
}

// featureTip returns the tip of the workstream's feature branch.
func featureTip(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	tip, _, err := workspaces(f.s.cfg, branchesDirectory, backendOf(t, f, stream)).Branch(context.Background(), featureBranch(stream))
	must(t, err)
	return tip
}

// conflictedDrift moves the feature branch and upstream to conflicting
// CODEOWNERS files and asks for a drift rebase. It returns the branch's tip,
// the upstream commit and the operation.
func conflictedDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	before := moveFeature(t, f, stream, map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	return before, upstream, requestDrift(t, d, stream)
}

func completeClaimedTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, q trace.QueuedTurn, outcome *coreadapter.Outcome) {
	t.Helper()
	f.sharedTurns().CompleteClaimedTurn(t, repository, stream, agent, q, outcome)
}
