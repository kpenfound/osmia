package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const inplaceGuardrailCheck = "dag://go/packages/tests/test"

// failsOnAttemptOne is a check runner that passes every candidate but the one
// that holds src/a1.go.
var failsOnAttemptOne = fixturekit.CheckFunc(func(_ context.Context, dir string) (api.CheckResult, error) {
	if _, err := os.Stat(filepath.Join(dir, "src", "a1.go")); err == nil {
		return api.CheckResult{ExitCode: 1, Output: fixturekit.FailedReport(inplaceGuardrailCheck)}, nil
	}
	return api.CheckResult{Output: fixturekit.PassedReport}, nil
})

func TestAnInPlaceAmendmentOfALimitOrACheckEndsAnAttemptKeptUnderTheOldOneOnceWhateverTheReviewerAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		amend   func(spec string) string
		cause   climb.Cause
		restart bool
	}{
		{"a lowered secondary maximum", func(spec string) string { return strings.Replace(spec, `"max": 150`, `"max": 100`, 1) }, climb.CauseLimits, false},
		{"a lowered secondary maximum across a restart", func(spec string) string { return strings.Replace(spec, `"max": 150`, `"max": 100`, 1) }, climb.CauseLimits, true},
		{"an added guardrail check the candidate fails", func(spec string) string {
			return strings.Replace(spec, `"guardrails": {`, `"guardrails": {"checks": ["`+inplaceGuardrailCheck+`"], `, 1)
		}, climb.CauseGuardrails, false},
		{"an added guardrail check the candidate fails across a restart", func(spec string) string {
			return strings.Replace(spec, `"guardrails": {`, `"guardrails": {"checks": ["`+inplaceGuardrailCheck+`"], `, 1)
		}, climb.CauseGuardrails, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newEvaluatorEngine()
			engine.ReviewChecks = failsOnAttemptOne
			engine.reply = func(n int) checks.CallResult {
				switch n {
				case 1, 3:
					return checks.CallResult{Stdout: benchOutputOf(100, 120)}
				case 2:
					return checks.CallResult{Stdout: benchOutputOf(101, 120)}
				case 4:
					return checks.CallResult{Stdout: benchOutputOf(90, 120)}
				}
				return checks.CallResult{Stdout: benchOutputOf(80, 90)}
			}
			original := strings.Replace(climbBuildingSpec, `"max": 200`, `"max": 150`, 1)
			original = strings.Replace(original, `"max_attempts": 20`, `"max_attempts": 2`, 1)
			c := newClimbFixtureWith(t, engine, true, nil, original)
			defer c.stop(t)
			c.playAttempt(1, map[string]string{"src/a1.go": "package src\n"}, "cache the parse")
			c.playAttempt(2, map[string]string{"src/a2.go": "package src\n"}, "batch the reads")

			var once sync.Once
			held, release := make(chan struct{}), make(chan struct{})
			c.holdReviews(func(ctx context.Context, turn string) error {
				if !strings.Contains(turn, "attempt-1-") {
					return nil
				}
				once.Do(func() { close(held) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			stream := c.ratified(t)
			c.diagnose(t, stream)
			c.awaitInner(t, stream, climb.InnerAwaitingApproval)
			card, _ := c.card(t)
			approval, err := c.c.ApproveClimb(context.Background(), stream, approvalOf(card))
			fixturekit.Must(t, err)
			waitFor(t, "attempt 1's review to start", held)
			open, found, err := climb.OpenAttempt(c.repository(), stream)
			fixturekit.Must(t, err)
			if !found || open.Number != 1 || open.Phase != climb.PhaseMeasured || open.Decision == nil || !open.Decision.Keep || open.Measurement == nil || open.Measurement.Score != 90 {
				t.Fatalf("open attempt %+v: kept at 90 under a limit of 150, its review pending", open)
			}
			evaluator := approval.Commit

			c.amendClimbSpec(t, stream, tc.amend(original))
			c.awaitAmendmentApplied(t, stream)
			close(release)
			if tc.restart {
				c.restart(t)
			}
			c.awaitAttempts(t, stream, 2)

			attempts := c.attemptsRecorded(t, stream)
			a1, a2 := attempts[0], attempts[1]
			if a1.Number != 1 || a1.Outcome != climb.OutcomeDiscarded || a1.Cause != tc.cause || a1.Commit != "" || a1.Scores == nil || a1.Scores.Primary != 90 {
				t.Fatalf("attempt 1 %+v", a1)
			}
			if a2.Outcome != climb.OutcomeKept || a2.Parent != evaluator || a2.Best != 100 {
				t.Fatalf("attempt 2 %+v starts from the evaluator commit %s", a2, evaluator)
			}
			branch := intake.FeatureBranch(stream)
			if tip := c.git(t, "rev-parse", branch); tip != a2.Commit {
				t.Fatalf("the climb branch is at %s, the last kept commit is %s", tip, a2.Commit)
			}
			if n := c.git(t, "rev-list", "--count", evaluator+".."+branch); n != "1" {
				t.Fatalf("%s commits on the climb branch since the evaluator; only attempt 2 lands", n)
			}
			if got := c.git(t, "rev-parse", "--verify", a1.Branch); got != a1.Candidate {
				t.Fatalf("the discarded branch holds %s, not the candidate %s", got, a1.Candidate)
			}
			docs, err := trace.Read[trace.Document](c.repository(), stream)
			fixturekit.Must(t, err)
			records := map[string]int{}
			for _, d := range docs {
				if strings.HasPrefix(d.Path, trace.ClimbAttemptsDir+"/") {
					records[d.Path]++
				}
			}
			if len(records) != 2 || records[trace.ClimbAttemptPath(1)] != 1 || records[trace.ClimbAttemptPath(2)] != 1 {
				t.Fatalf("terminal records %v", records)
			}
			if n := engine.count(); n != 5 {
				t.Fatalf("the evaluator ran %d times: 3 calibration runs and one measurement of each attempt", n)
			}
			th, err := c.repository().Thread(stream, execution.ClimbMasonAgent)
			fixturekit.Must(t, err)
			var turns []string
			for _, q := range th.Turns {
				turns = append(turns, q.Request.TurnID)
			}
			if fmt.Sprint(turns) != "[climb-mason-1-1 climb-mason-2-1]" {
				t.Fatalf("the climb's mason thread ran %v", turns)
			}
		})
	}
}
