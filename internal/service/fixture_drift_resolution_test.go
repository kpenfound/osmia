package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

func awaitResolution(t *testing.T, s *Service, d drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	if result, err := attemptOperation(t, s, d.repository, stream, op, d); !errors.Is(err, errDriftAwaits) || !strings.Contains(err.Error(), want) {
		t.Fatalf("the drift rebase did not await %q: %+v %v", want, result, err)
	}
}

func completeDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return intakefixture.CompleteDriftTurn(t, f.sharedTurns(), repository, stream, agent, outcome)
}

func resolvedDone(outcome string) *coreadapter.Outcome {
	data, _ := json.Marshal(MasonReport{Outcome: outcome})
	return &coreadapter.Outcome{Status: execution.MasonDone, Report: string(data)}
}

func driftVerdict(t *testing.T, verdict UnitVerdict) *coreadapter.Outcome {
	t.Helper()
	return intakefixture.VerdictOutcome(t, verdict, execution.VerdictOutcome)
}

var (
	approvedResolution = UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
)

func turnIDs(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent string) []string {
	t.Helper()
	return intakefixture.TurnIDs(t, repository, stream, agent)
}

func resolutionWorkspace(t *testing.T, f *shedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	return intakefixture.Workspace(t, workspaces(f.s.cfg, driftsDirectory, backendOf(t, f, stream)), stream)
}

func featureTip(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	tip, _, err := workspaces(f.s.cfg, branchesDirectory, backendOf(t, f, stream)).Branch(context.Background(), featureBranch(stream))
	intakefixture.Must(t, err)
	return tip
}

func conflictedDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	before := moveFeature(t, f, stream, map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	return before, upstream, requestDrift(t, d, stream)
}
