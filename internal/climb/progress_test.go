package climb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func progressSpec(d Direction) Spec {
	target := 70.0
	return Spec{Version: 1, Goal: "Make the parser faster.", Evaluator: "parse-bench", Repeats: 1,
		Metric: MetricSpec{Name: "wall_ms", Direction: d, MinImprovement: 5},
		Stop:   StopRules{MaxAttempts: 20, MaxCostUSD: 10, MaxWallClock: "2h", Plateau: 5, Target: &target}}
}

func progressAttempts() []Attempt {
	start := fixturekit.DemoStart
	kept := func(n int, best, score float64) Attempt {
		a := Attempt{Number: n, Parent: "p", Best: best, Commit: "c", Hypothesis: "h", Outcome: OutcomeKept, StartedAt: start.Add(time.Duration(n-1) * time.Minute), FinishedAt: start.Add(time.Duration(n) * time.Minute)}
		a.Scores = &Scores{Primary: score}
		return a
	}
	slower := Attempt{Number: 2, Parent: "p", Best: 90, Hypothesis: "h", Outcome: OutcomeDiscarded, Cause: CauseNoImprovement, Scores: &Scores{Primary: 95}, StartedAt: start.Add(time.Minute), FinishedAt: start.Add(2 * time.Minute)}
	broken := Attempt{Number: 3, Parent: "p", Best: 90, Hypothesis: "h", Outcome: OutcomeDiscarded, Cause: CauseNoCandidate, StartedAt: start.Add(2 * time.Minute), FinishedAt: start.Add(3 * time.Minute)}
	return []Attempt{kept(1, 100, 90), slower, broken}
}

func TestProgressReadsTheClimbForAMetricThatFalls(t *testing.T) {
	t.Parallel()
	cal := &Calibration{Verdict: Calibrated, Baseline: 100, Spread: 2, Units: map[string]string{"wall_ms": "ms"}}
	run := &Running{Attempt: 4, Hypothesis: "inline the lexer", Phase: kinds.PhaseMeasuring, Run: 2, Runs: 3, StartedAt: fixturekit.DemoStart.Add(3 * time.Minute)}
	p := BuildProgress(ProgressInput{Spec: progressSpec(Lower), Calibration: cal, Running: run, Attempts: progressAttempts(), CostUSD: 3.5, Now: fixturekit.DemoStart.Add(4 * time.Minute)})
	if *p.Baseline != 100 || *p.Best != 90 || p.BestAttempt != 1 || *p.Change != -10 || *p.ChangePercent != -10 || *p.NoiseBand != 2 || p.Unit != "ms" || *p.Target != 70 {
		t.Fatalf("progress %+v", p)
	}
	if p.Summary != "3 attempts \u00b7 attempt 4 measuring 2/3 \u00b7 best \u221210.0%" {
		t.Fatalf("summary %q", p.Summary)
	}
	if p.Running == nil || p.Running.Words != "measuring 2/3" || p.Running.Attempt != 4 {
		t.Fatalf("running %+v", p.Running)
	}
	b := p.Budgets
	if b.Attempts != (Meter{Used: 3, Max: 20}) || b.CostUSD != (Meter{Used: 3.5, Max: 10}) || b.SinceImprovement != (Meter{Used: 2, Max: 5}) || b.WallClockSeconds.Max != 7200 || b.WallClockSeconds.Used != 240 {
		t.Fatalf("budgets %+v", b)
	}
	classes := []Class{p.Points[0].Class, p.Points[1].Class, p.Points[2].Class}
	if classes[0] != ClassKept || classes[1] != ClassDiscarded || classes[2] != ClassFailed || p.Points[2].Scored || p.Points[2].Y != 90 || p.Points[1].Y != 95 || p.Points[1].Best != 90 {
		t.Fatalf("points %+v", p.Points)
	}
	want := []float64{-100, -90, -90, -90}
	for i, v := range want {
		if p.Spark[i] != v {
			t.Fatalf("spark %v, want %v: a falling metric is drawn negated so a rising line improves", p.Spark, want)
		}
	}
}

func TestProgressOfAMetricThatRisesIsNotNegated(t *testing.T) {
	t.Parallel()
	a := progressAttempts()[0]
	a.Best, a.Scores.Primary = 100, 112
	p := BuildProgress(ProgressInput{Spec: progressSpec(Higher), Calibration: &Calibration{Verdict: Calibrated, Baseline: 100}, Attempts: []Attempt{a}, Now: a.FinishedAt})
	if *p.ChangePercent != 12 || p.Spark[0] != 100 || p.Spark[1] != 112 || !strings.HasSuffix(p.Summary, "best +12.0%") {
		t.Fatalf("progress %+v", p)
	}
}

func TestProgressBeforeAnyAttemptHasNoBestAndNoSparkline(t *testing.T) {
	t.Parallel()
	p := BuildProgress(ProgressInput{Spec: progressSpec(Lower), Now: fixturekit.DemoStart})
	if p.Baseline != nil || p.Best != nil || p.Spark != nil || p.Summary != "0 attempts" || p.Running != nil || len(p.Points) != 0 {
		t.Fatalf("progress %+v", p)
	}
	idle := BuildProgress(ProgressInput{Spec: progressSpec(Lower), Calibration: &Calibration{Verdict: Calibrated, Baseline: 100, Spread: 1}, Running: &Running{Attempt: 1}, Now: fixturekit.DemoStart})
	if idle.Running != nil || idle.Summary != "0 attempts · no improvement yet" {
		t.Fatalf("a cleared running state still shows: %+v", idle)
	}
}

func TestProgressNamesTheAttemptInFlightByItsPhase(t *testing.T) {
	t.Parallel()
	for phase, want := range map[string]string{
		kinds.PhaseMason: "0 attempts · attempt 1 working", kinds.PhaseGuardrails: "0 attempts · attempt 1 guardrails",
		kinds.PhaseMeasuring: "0 attempts · attempt 1 measuring", kinds.PhaseReviewing: "0 attempts · attempt 1 reviewing",
	} {
		p := BuildProgress(ProgressInput{Spec: progressSpec(Lower), Running: &Running{Attempt: 1, Phase: phase, StartedAt: fixturekit.DemoStart}, Now: fixturekit.DemoStart})
		if p.Summary != want {
			t.Errorf("%s: %q, want %q", phase, p.Summary, want)
		}
	}
}

func TestRunningStateIsRecordedAsRevisionsAndReadBack(t *testing.T) {
	t.Parallel()
	repo := newAttemptStore(t)
	ctx := context.Background()
	if _, found, err := ReadRunning(repo, fixturekit.Stream); err != nil || found {
		t.Fatalf("an unrecorded state: %v, %v", found, err)
	}
	first := Running{Attempt: 1, Hypothesis: "h", Phase: kinds.PhaseMason, StartedAt: fixturekit.DemoStart}
	fixturekit.Must(t, RecordRunning(ctx, repo, fixturekit.Stream, first, fixturekit.DemoStart))
	before, err := repo.CommitCount(string(fixturekit.Stream))
	fixturekit.Must(t, err)
	fixturekit.Must(t, RecordRunning(ctx, repo, fixturekit.Stream, first, fixturekit.DemoStart))
	if after, _ := repo.CommitCount(string(fixturekit.Stream)); after != before {
		t.Fatalf("recording the same state again committed (%d, %d)", before, after)
	}
	next := first
	next.Phase = kinds.PhaseMeasuring
	next.Run, next.Runs = 1, 3
	fixturekit.Must(t, RecordRunning(ctx, repo, fixturekit.Stream, next, fixturekit.DemoStart.Add(time.Second)))
	got, found, err := ReadRunning(repo, fixturekit.Stream)
	if err != nil || !found || got.Phase != kinds.PhaseMeasuring || got.Run != 1 || got.Runs != 3 || !got.Active() {
		t.Fatalf("%+v, %v, %v", got, found, err)
	}
	fixturekit.Must(t, RecordRunning(ctx, repo, fixturekit.Stream, Running{}, fixturekit.DemoStart.Add(2*time.Second)))
	if got, _, _ := ReadRunning(repo, fixturekit.Stream); got.Active() {
		t.Fatalf("a cleared state is active: %+v", got)
	}
}

func TestProgressReadsTheBaselineAndNoiseFromTheCalibrationRecord(t *testing.T) {
	t.Parallel()
	repo := newAttemptStore(t)
	rec := CalibrationRecord{Evaluator: "parse-bench", Primary: "wall_ms", SpecRevision: 1, At: fixturekit.DemoStart,
		Calibration: Calibration{Verdict: Calibrated, Baseline: 100, Spread: 2, Units: map[string]string{"wall_ms": "ms"}}}
	data, err := json.MarshalIndent(rec, "", "  ")
	fixturekit.Must(t, err)
	doc := trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.ClimbCalibrationID, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: InnerActor, Cause: "climb-calibration-1"},
		Path:   trace.ClimbCalibrationPath, Content: string(data) + "\n",
	}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	got, _, found, err := ReadCalibration(repo, fixturekit.Stream)
	if err != nil || !found {
		t.Fatalf("%v, %v", found, err)
	}
	p := BuildProgress(ProgressInput{Spec: progressSpec(Lower), Calibration: &got.Calibration, Now: fixturekit.DemoStart})
	if p.Baseline == nil || *p.Baseline != 100 || p.NoiseBand == nil || *p.NoiseBand != 2 || p.Unit != "ms" {
		t.Fatalf("progress %+v", p)
	}
}
