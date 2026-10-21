package executionjourney

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/questions"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func (f *architectFixture) runs() []string {
	return f.scenario().Runs()
}

func (f *crossRepoRatifyFixture) scriptDrafts(t *testing.T) {
	t.Helper()
	intakefixture.ScriptCrossRepoDrafts(t, f.engine, func() (config.WorkstreamID, config.WorkstreamID) { return f.stream1, f.stream2 })
}

type FailureStreak = api.FailureStreak

var failureHistory = execution.FailureHistory
var failureContestID = execution.FailureContestID

const turnRetries = service.TestTurnRetries

type reviewerReports = execution.ReviewerReports

func completeClaimedTurn(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, a string, q trace.QueuedTurn, o *coreadapter.Outcome) {
	t.Helper()
	f.sharedTurns().CompleteClaimedTurn(t, r, w, a, q, o)
}
func chiefRecordsAnswer(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, id, text, cite string) {
	t.Helper()
	f.sharedTurns().ChiefRecordsAnswer(t, r, w, id, text, cite)
}

func claimOldestTurn(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, a string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.ClaimOldestTurn(t, f.sharedTurns(), r, w, a)
}

var askTool = intakefixture.AskTool

func (s *Service) answers(cfg *config.Config, r *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, r)
}

func chiefAnswers(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, id, text, cite string) {
	t.Helper()
	chiefRecordsAnswer(t, f, r, w, id, text, cite)
	must(t, f.s.answers(f.s.cfg, r).Pass(context.Background()))
}

var latestUnitTransition = execution.LatestUnitTransition
var landingSubject = execution.LandingSubject
var errCrashTurn = intakefixture.ErrCrashTurn
