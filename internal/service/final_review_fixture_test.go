package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newFinalFixture seeds a sealed validPlan with a committee runner and no
// masons. It returns the stopped service, workstream, open trace and assembly
// controller. Intake, debate and ratification are covered by lifecycle tests.
func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	return newFinalFixtureWith(t, key, "")
}

// newFinalFixtureWith is newFinalFixture with extra configuration appended.
func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.buildFixture().SeedGit(t, key)
	return f, stream, repository, &finalReviewer{s: f.s, repository: repository}
}

// mergeDirectly commits files onto the feature branch as the unit's landing
// and records the unit merged, as a landing does, and returns the new tip.
func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, moveFeature(t, f, stream, files), foremanActor, UnitMerged)
}

// advanceUpstream commits files on upstream's main and returns the commit.
func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.clone, files)
}

// finalOperations returns the workstream's final review operations.
func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.FinalOperations(t, repository, stream, FinalReviewAction)
}

var streamDocuments = intakefixture.StreamDocuments

var transitionByID = intakefixture.TransitionByID

var chiefEvent = intakefixture.ChiefEvent

var noticeOf = intakefixture.NoticeOf

// assembleBoth merges both units of validPlan, checking that the workstream
// stays building while one is unmerged, and asks for the first final review.
// It returns the feature branch tip and the review's operation.
func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, AssembledState, FinalReviewAction)
}

// finalTurn installs the fake committee member's turn of final review k.
func (f *shedFixture) finalTurn(k, attempt int, run func(context.Context, *mcp.ClientSession) error) string {
	return intakefixture.FinalTurn(f.engine, finalTurnID(k, committeeAgent(1), attempt), run)
}
