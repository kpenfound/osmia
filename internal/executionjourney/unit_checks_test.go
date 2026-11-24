package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func newChecksFixture(t *testing.T, key string, checker checks.ReviewChecks) (*journey.ShedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	f.S.Options.ReviewChecks = checker
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := journey.SeedBuild(t, f, key, intakefixture.IndependentPlan, config.WorkspacesGit, base)
	journey.SeedChecking(t, f, repository, stream, "resume", journey.ResumeReport)
	return f, stream, repository
}

func useJevProvider(s *journey.Service, p *jevtest.Provider) {
	s.Jev = intakefixture.CheckJudge(s.Now, p)
	service.InspectForTest(s.Core()).SetJudge(s.Jev)
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
		f.Clock.Mu.Lock()
		f.Clock.At = f.Clock.At.Add(execution.CheckRetryDelay + time.Minute)
		f.Clock.Mu.Unlock()
	}
	for range execution.MaxIncompleteChecks {
		journey.RunChecks(t, f.S, repository, stream)
		advance()
	}
	journey.RunChecks(t, f.S, repository, stream)
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
	journey.RunChecks(t, f.S, repository, stream)
	if ran := checker.Runs(); len(ran) != execution.MaxIncompleteChecks {
		t.Fatalf("a contested unit's checks ran again: %d runs", len(ran))
	}

	fixturekit.Must(t, repository.Close())
	f.Start(t)
	defer f.Stop(t)
	if _, apiErr := f.S.RuleContested(ctx, string(stream), "resume", api.ContestedRulingRequest{Decision: "review", Note: "The engine is back."}); apiErr != nil {
		t.Fatalf("ruling review: %+v", apiErr)
	}
	intakefixture.Soon(t, "the ruled unit to pass its checks and reach review", func() bool {
		runs, err := execution.CheckRuns(f.Repository(), stream)
		fixturekit.Must(t, err)
		return len(runs) == execution.MaxIncompleteChecks+1 && runs[execution.MaxIncompleteChecks].Status == checks.ChecksPassed && intakefixture.UnitState(t, f.Repository(), stream, "resume") == intake.UnitReviewing
	})
}
