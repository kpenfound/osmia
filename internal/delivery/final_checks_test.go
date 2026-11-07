package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// finalChecksFixture returns a final reviewer whose checks are checks and the
// report of a commit for it to check.
func finalChecksFixture(t *testing.T, checks checkrun.ReviewChecks) (*FinalReviewer, *config.Config, FinalReport) {
	t.Helper()
	cfg, repository := deliveryTrace(t)
	home := filepath.Dir(cfg.Project.Clone)
	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(fixturekit.DemoGit(t, home, append([]string{"-C", cfg.Project.Clone}, args...)...))
	}
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.test")
	git("commit", "--allow-empty", "-qm", "base")
	base := git("rev-parse", "HEAD")
	file := filepath.Join(cfg.Project.Clone, fixturekit.MasonWrote)
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(file), 0700))
	fixturekit.Must(t, os.WriteFile(file, []byte("package trace\n"), 0600))
	git("add", ".")
	git("commit", "-qm", "candidate")
	commit := git("rev-parse", "HEAD")

	controller := New(Dependencies{
		Now:    func() time.Time { return fixturekit.DemoStart },
		Step:   func(string) error { return nil },
		Checks: checks,
		AcquireReviewCheckSlot: func(context.Context, config.ProjectID, config.WorkstreamID, int, string) (func(), error) {
			return func() {}, nil
		},
	})
	reviewer := &FinalReviewer{S: controller, Repository: repository}
	report := FinalReport{Review: 1, Operation: "review", Commit: commit, Upstream: &seal.Base{Commit: base}}
	return reviewer, cfg, report
}

func TestFinalChecksNoteFlakyChecksOnAPassingRun(t *testing.T) {
	t.Parallel()
	const link = "dag://?check=test&go-test=TestFlaky"
	checks := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: CheckResult{Output: fixturekit.FailedReport(link) + "CHECKS  ✔ 1 passed\n✔ " + link + " 1s OK\n", Flaky: []string{link}}}}}
	reviewer, cfg, report := finalChecksFixture(t, checks)
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if run.Status != checkrun.ChecksPassed || len(run.Failed) != 0 || !slices.Equal(run.Flaky, []string{link}) {
		t.Fatalf("final checks: %+v", run)
	}
	if evidence := FinalCheckEvidence(run); !strings.Contains(evidence, checkrun.DescribeFlaky([]string{link})) || !strings.Contains(evidence, "[passed on rerun]") {
		t.Fatalf("evidence lacks the flake:\n%s", evidence)
	}
}

func TestFinalChecksRecordsLifecycleReportFailure(t *testing.T) {
	t.Parallel()
	raw := fixturekit.LifecycleFailureOutput()
	checks := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: CheckResult{ExitCode: 1, Output: raw}}}}
	reviewer, cfg, report := finalChecksFixture(t, checks)
	commit, repository := report.Commit, reviewer.Repository
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if run.Status != checkrun.ChecksFailed || run.Error != "" || run.ExitCode != 1 ||
		!slices.Equal(run.Failed, []string{"dag://?check=test&go-package=internal/lifecycle"}) ||
		!strings.Contains(run.Output, "lifecycle_test.go:42: expected pending change to apply") {
		t.Fatalf("final checks: %+v", run)
	}
	wantCommand := []string{"dagger", "check", "--progress=report", "--fail-fast", "--parallel", "4"}
	if !slices.Equal(run.Command, wantCommand) {
		t.Fatalf("command: %q", run.Command)
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	var recorded, captured bool
	for _, doc := range docs {
		switch doc.Path {
		case FinalChecksPath(1):
			var saved FinalCheckRun
			fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &saved))
			if !reflect.DeepEqual(saved, run) {
				t.Fatalf("recorded result differs: %+v", saved)
			}
			recorded = true
		case run.OutputPath:
			if doc.Content != raw || doc.Revision != run.OutputRevision {
				t.Fatal("complete captured output or its revision was lost")
			}
			captured = true
		}
	}
	if !recorded || !captured {
		t.Fatalf("recorded result=%t captured output=%t", recorded, captured)
	}
	count, _, err := reviewer.IncompleteChecks(stream, 1, commit)
	fixturekit.Must(t, err)
	if count != 0 {
		t.Fatalf("failed check entered incomplete retry: %d", count)
	}
	cached, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(cached, run) || len(checks.Runs()) != 1 {
		t.Fatal("completed failure was rerun")
	}
}

func TestFinalChecksRunFallbackPicks(t *testing.T) {
	t.Parallel()
	cfg, repository := deliveryTrace(t)
	home := filepath.Dir(cfg.Project.Clone)
	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(fixturekit.DemoGit(t, home, append([]string{"-C", cfg.Project.Clone}, args...)...))
	}
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.test")
	git("commit", "--allow-empty", "-qm", "base")
	base := git("rev-parse", "HEAD")
	fixturekit.Must(t, os.WriteFile(filepath.Join(cfg.Project.Clone, "changed.txt"), []byte("change"), 0600))
	git("add", ".")
	git("commit", "-qm", "candidate")
	commit := git("rev-parse", "HEAD")
	checks := &fixturekit.FakeChecks{Links: []string{"dag+check://a", "dag+check://b", "dag+check://c"}, Outcomes: fixturekit.Passing()}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5, CheckFallbackPicks: 2}
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.1), "check-1": jevtest.Noul(.4), "check-2": jevtest.Noul(.3)}}}}}
	controller := New(Dependencies{Now: func() time.Time { return fixturekit.DemoStart }, Step: func(string) error { return nil }, Checks: checks, Jev: &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }}, AcquireReviewCheckSlot: func(context.Context, config.ProjectID, config.WorkstreamID, int, string) (func(), error) {
		return func() {}, nil
	}})
	reviewer := &FinalReviewer{S: controller, Repository: repository}
	report := FinalReport{Review: 1, Operation: "review", Commit: commit, Upstream: &seal.Base{Commit: base}}
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	want := []string{"dag+check://b", "dag+check://c"}
	if ran := checks.Runs(); len(ran) != 1 || !slices.Equal(ran[0], want) || run.Selection.FallbackPicks != 2 || !slices.Equal(run.Selection.Links, want) {
		t.Fatalf("ran %v, selection %+v", ran, run.Selection)
	}
}

func TestFinalCheckRetryReusesBreadthDepthAndRanking(t *testing.T) {
	t.Parallel()
	checks := fixturekit.SmartChecks()
	checks.Outcomes = append([]fixturekit.CheckOutcome{{Result: CheckResult{ExitCode: -1}, Err: errors.New("engine disconnected")}}, fixturekit.Passing()...)
	reviewer, cfg, report := finalChecksFixture(t, checks)
	now := fixturekit.DemoStart
	reviewer.S.Now = func() time.Time { return now }
	p := &jevtest.Provider{Results: []jevtest.Result{
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"breadth-0": jevtest.Noul(.9), "breadth-1": jevtest.Noul(.01), "breadth-2": jevtest.Noul(.9)}, Usage: systemone.Usage{CostUSD: .01, CostKnown: true}}},
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"depth-0": jevtest.Choice("go-package", .8, .8, "go-test")}, Usage: systemone.Usage{CostUSD: .02, CostKnown: true}}},
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.9), "check-1": jevtest.Noul(.9), "check-2": jevtest.Noul(.9), "check-3": jevtest.Noul(.9)}, Usage: systemone.Usage{CostUSD: .03, CostKnown: true}}},
	}}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "1s", CheckThreshold: .5}
	newJudge := func() *jev.Judge {
		return &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: reviewer.S.Now}
	}
	reviewer.S.Jev = newJudge()
	first, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	if err == nil {
		t.Fatal("incomplete final checks did not request a retry")
	}
	if first.Status != checkrun.ChecksIncomplete || first.Selection.BreadthJudgment == "" || first.Selection.DepthJudgment == "" || first.Selection.Judgment == "" || len(p.Requests()) != 3 {
		t.Fatalf("first run %+v selection %+v requests %d", first, first.Selection, len(p.Requests()))
	}
	// A restarted service retries the same review after its check backoff.
	reviewer.S.Jev = newJudge()
	now = now.Add(FinalCheckDelay(1))
	second, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if second.Status != checkrun.ChecksPassed || second.Attempt != 2 || !reflect.DeepEqual(second.Selection, first.Selection) || len(p.Requests()) != 3 {
		t.Fatalf("retry %+v selection %+v requests %d", second, second.Selection, len(p.Requests()))
	}
	want := []string{fixturekit.SmartStandalone, fixturekit.SmartPackageA, fixturekit.SmartPackageB, fixturekit.SmartUncovered}
	slices.Sort(want)
	if runs := checks.Runs(); len(runs) != 2 || !slices.Equal(runs[0], want) || !slices.Equal(runs[1], want) {
		t.Fatalf("runner %q", runs)
	}
	costs, err := trace.Read[trace.Cost](reviewer.Repository, stream)
	fixturekit.Must(t, err)
	if len(costs) != 3 {
		t.Fatalf("retry billed again: %+v", costs)
	}
	ids := map[string]bool{}
	for _, cost := range costs {
		if cost.Entry.Scope.Role != "jev" || !cost.Entry.Usage.CostKnown {
			t.Fatalf("cost %+v", cost)
		}
		ids[cost.Entry.AttemptID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("stages share a ledger identity: %+v", costs)
	}
}
