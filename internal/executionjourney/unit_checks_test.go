package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"

	strings "strings"
	testing "testing"
	time "time"

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

func newChecksFixture(t *testing.T, key string, checks ReviewChecks) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	f.s.options.reviewChecks = checks
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func boostJev(s *Service, p *jevtest.Provider) {
	s.jev = intakefixture.CheckJudge(s.now, p)
	service.InspectForTest(s.core()).SetJudge(s.jev)
}

var unitState = intakefixture.UnitState

func onlyRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) UnitCheckRun {
	t.Helper()
	return intakefixture.OnlyRun(t, repository, stream, checkRuns)
}

func currentReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) UnitReport {
	t.Helper()
	return intakefixture.CurrentReport[UnitReport](t, repository, stream, unit, reportDocument)
}

func TestIncompleteChecksAreBoundedAndContestTheUnit(t *testing.T) {
	t.Parallel()
	var outcomes []checkOutcome
	for range maxIncompleteChecks {
		outcomes = append(outcomes, checkOutcome{Err: errors.New("engine unreachable")})
	}
	checks := &fakeChecks{Outcomes: append(outcomes, checkOutcome{Result: CheckResult{Output: passedReport}})}
	f, stream, repository := newChecksFixture(t, "checks-bounded", checks)
	ctx := context.Background()
	advance := func() {
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(checkRetryDelay + time.Minute)
		f.clock.Mu.Unlock()
	}
	for range maxIncompleteChecks {
		runChecks(t, f.s, repository, stream)
		advance()
	}
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitContested {
		t.Fatalf("unit is %s after %d incomplete runs", got, maxIncompleteChecks)
	}
	if ran := checks.Runs(); len(ran) != maxIncompleteChecks {
		t.Fatalf("checks ran %d times", len(ran))
	}
	contest := transitionByID(t, repository, stream, checkDocumentID("resume", maxIncompleteChecks)+"-contested")
	if !checksContest(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("%d check runs in a row", maxIncompleteChecks)) || !strings.Contains(contest.Reason, "engine unreachable") {
		t.Fatalf("the contest %+v", contest)
	}
	advance()
	runChecks(t, f.s, repository, stream)
	if ran := checks.Runs(); len(ran) != maxIncompleteChecks {
		t.Fatalf("a contested unit's checks ran again: %d runs", len(ran))
	}

	must(t, repository.Close())
	f.start(t)
	defer f.stop(t)
	if _, api := f.s.ruleContested(ctx, string(stream), "resume", ContestedRulingRequest{Decision: "review", Note: "The engine is back."}); api != nil {
		t.Fatalf("ruling review: %+v", api)
	}
	deadline := time.Now().Add(demoTimeout)
	for {
		runs, err := checkRuns(f.repository(), stream)
		must(t, err)
		if len(runs) == maxIncompleteChecks+1 && runs[maxIncompleteChecks].Status == ChecksPassed && unitState(t, f.repository(), stream, "resume") == UnitReviewing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the ruling the check runs are %+v and the unit is %s", runs, unitState(t, f.repository(), stream, "resume"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
