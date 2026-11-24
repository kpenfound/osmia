package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	strings "strings"
	testing "testing"
	time "time"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func runChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &checkers{masons: newMasonController(s, repository)}, service.CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, repository, stream, op, r)
	})
}

func newChecksFixture(t *testing.T, key string, checker checks.ReviewChecks) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	f.s.options.reviewChecks = checker
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func useJevProvider(s *Service, p *jevtest.Provider) {
	s.jev = intakefixture.CheckJudge(s.now, p)
	service.InspectForTest(s.core()).SetJudge(s.jev)
}

func onlyRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) execution.UnitCheckRun {
	t.Helper()
	return intakefixture.OnlyRun(t, repository, stream, execution.CheckRuns)
}

func currentReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) execution.UnitReport {
	t.Helper()
	return intakefixture.CurrentReport[execution.UnitReport](t, repository, stream, unit, execution.ReportDocument)
}

func TestIncompleteChecksAreBoundedAndContestTheUnit(t *testing.T) {
	t.Parallel()
	var outcomes []fixturekit.CheckOutcome
	for range execution.MaxIncompleteChecks {
		outcomes = append(outcomes, fixturekit.CheckOutcome{Err: errors.New("engine unreachable")})
	}
	checker := &fixturekit.FakeChecks{Outcomes: append(outcomes, fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}})}
	f, stream, repository := newChecksFixture(t, "checks-bounded", checker)
	ctx := context.Background()
	advance := func() {
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(execution.CheckRetryDelay + time.Minute)
		f.clock.Mu.Unlock()
	}
	for range execution.MaxIncompleteChecks {
		runChecks(t, f.s, repository, stream)
		advance()
	}
	runChecks(t, f.s, repository, stream)
	if got := intakefixture.UnitState(t, repository, stream, "resume"); got != intake.UnitContested {
		t.Fatalf("unit is %s after %d incomplete runs", got, execution.MaxIncompleteChecks)
	}
	if ran := checker.Runs(); len(ran) != execution.MaxIncompleteChecks {
		t.Fatalf("checks ran %d times", len(ran))
	}
	contest := intakefixture.TransitionByID(t, repository, stream, execution.CheckDocumentID("resume", execution.MaxIncompleteChecks)+"-contested")
	if !execution.ChecksContest(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("%d check runs in a row", execution.MaxIncompleteChecks)) || !strings.Contains(contest.Reason, "engine unreachable") {
		t.Fatalf("the contest %+v", contest)
	}
	advance()
	runChecks(t, f.s, repository, stream)
	if ran := checker.Runs(); len(ran) != execution.MaxIncompleteChecks {
		t.Fatalf("a contested unit's checks ran again: %d runs", len(ran))
	}

	fixturekit.Must(t, repository.Close())
	f.start(t)
	defer f.stop(t)
	if _, apiErr := f.s.ruleContested(ctx, string(stream), "resume", api.ContestedRulingRequest{Decision: "review", Note: "The engine is back."}); apiErr != nil {
		t.Fatalf("ruling review: %+v", apiErr)
	}
	intakefixture.Soon(t, "the ruled unit to pass its checks and reach review", func() bool {
		runs, err := execution.CheckRuns(f.repository(), stream)
		fixturekit.Must(t, err)
		return len(runs) == execution.MaxIncompleteChecks+1 && runs[execution.MaxIncompleteChecks].Status == checks.ChecksPassed && intakefixture.UnitState(t, f.repository(), stream, "resume") == intake.UnitReviewing
	})
}
