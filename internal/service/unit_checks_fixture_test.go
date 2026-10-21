package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"strings"

	testing "testing"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"

	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func runChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &checkers{masons: newMasonController(s, repository)}, CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, repository, stream, op, r)
	})
}

func boostJev(s *Service, p *jevtest.Provider) {
	s.jev = intakefixture.CheckJudge(s.now, p)
}

var unitState = intakefixture.UnitState

func newChecksFixture(t *testing.T, key string, checks ReviewChecks) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	f.s.options.reviewChecks = checks
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func onlyRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) UnitCheckRun {
	t.Helper()
	return intakefixture.OnlyRun(t, repository, stream, checkRuns)
}

func currentReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) UnitReport {
	t.Helper()
	return intakefixture.CurrentReport[UnitReport](t, repository, stream, unit, reportDocument)
}

func reviewTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) trace.TurnRequest {
	t.Helper()
	r := &reviewers{masons: newMasonController(f.s, repository)}
	must(t, r.Pass(context.Background()))
	th, err := repository.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	if len(th.Turns) == 0 {
		t.Fatal("no review turn was queued")
	}
	return th.Turns[len(th.Turns)-1].Request
}

var errFailTurn = intakefixture.ErrFailTurn
