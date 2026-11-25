package climb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func newAttemptStore(t *testing.T) *trace.Repository {
	t.Helper()
	_, _, repo := fixturekit.Activity(t)
	t.Cleanup(func() { repo.Close() })
	fixturekit.Must(t, repo.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, fixturekit.OwnerActor))
	return repo
}

func keptAttempt(n int) Attempt {
	m := Aggregate(expectations(), 2, []Run{scoredRun(1, 88, 10), scoredRun(2, 90, 10)})
	a := Attempt{
		Number: n, Parent: "parentcommit", Best: 100, Commit: "landedcommit", Candidate: "candidatecommit",
		Hypothesis: "cache the parsed workload", ChangeSummary: "adds a parse cache", Expected: "-10% wall time",
		Diff:    DiffSummary{Files: []string{"internal/parse.go"}, Summary: "1 file changed, 20 insertions(+)"},
		Outcome: OutcomeKept, StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart.Add(time.Minute),
	}
	a.Measured(m, Decide(Candidate{Measurement: m, Best: 100, Direction: Lower, MinImprovement: 5, ChecksGreen: true}))
	return a
}

func discardedAttempt(n int) Attempt {
	m := Aggregate(expectations(), 3, []Run{scoredRun(1, 99, 10), {Number: 2, Status: RunInvalid, Reason: "wrong answer", Output: `{"schema":1,"valid":false,"reason":"wrong answer","metrics":{}}`}})
	a := Attempt{Number: n, Parent: "parentcommit", Best: 100, Candidate: "candidatecommit", Branch: "osmia/climb/attempt-2", Hypothesis: "skip validation", Outcome: OutcomeDiscarded, Cause: CauseInvalid, Reason: m.Reason, StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart.Add(time.Minute)}
	a.Measured(m, Decide(Candidate{Measurement: m}))
	return a
}

func TestAttemptRecordsRoundTripThroughTheWorkstreamTrace(t *testing.T) {
	t.Parallel()
	repo := newAttemptStore(t)
	ctx := context.Background()
	kept, discarded := keptAttempt(1), discardedAttempt(2)
	for _, a := range []Attempt{discarded, kept} {
		fixturekit.Must(t, RecordAttempt(ctx, repo, fixturekit.Stream, a))
	}
	got, err := ReadAttempts(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("attempts in order: %+v", got)
	}
	k := got[0]
	if k.Parent != "parentcommit" || k.Hypothesis != "cache the parsed workload" || k.Outcome != OutcomeKept || k.Scores == nil || k.Scores.Primary != 89 || *k.Change != -11 || *k.Improvement != 11 || *k.ChangePct != -11 || k.Diff.Files[0] != "internal/parse.go" || k.Measurement == nil || len(k.Measurement.Runs) != 2 || k.Commit != "landedcommit" {
		t.Fatalf("kept attempt: %+v", k)
	}
	d := got[1]
	if d.Outcome != OutcomeDiscarded || d.Cause != CauseInvalid || d.Scores != nil || d.Change != nil || d.Measurement == nil || d.Measurement.Valid || d.Measurement.Runs[1].Output == "" || d.Branch != "osmia/climb/attempt-2" {
		t.Fatalf("discarded attempt: %+v", d)
	}
	one, found, err := ReadAttempt(repo, fixturekit.Stream, 2)
	if err != nil || !found || one.Hypothesis != "skip validation" {
		t.Fatalf("%+v, %v, %v", one, found, err)
	}
	if _, found, err := ReadAttempt(repo, fixturekit.Stream, 3); err != nil || found {
		t.Fatalf("an unrecorded attempt: %v, %v", found, err)
	}
}

func TestAnAttemptIsRecordedOnceAndNeverRewritten(t *testing.T) {
	t.Parallel()
	repo := newAttemptStore(t)
	ctx := context.Background()
	a := discardedAttempt(1)
	fixturekit.Must(t, RecordAttempt(ctx, repo, fixturekit.Stream, a))
	before, err := repo.CommitCount(string(fixturekit.Stream))
	fixturekit.Must(t, err)
	fixturekit.Must(t, RecordAttempt(ctx, repo, fixturekit.Stream, a))
	after, err := repo.CommitCount(string(fixturekit.Stream))
	fixturekit.Must(t, err)
	if after != before {
		t.Fatalf("recording the same attempt again committed (%d, %d)", before, after)
	}
	other := a
	other.Cause, other.Reason = CauseNoImprovement, "different"
	if err := RecordAttempt(ctx, repo, fixturekit.Stream, other); !errors.Is(err, trace.ErrConflict) {
		t.Fatalf("a different record for the same number: %v", err)
	}
	got, _, _ := ReadAttempt(repo, fixturekit.Stream, 1)
	if got.Cause != CauseInvalid {
		t.Fatalf("the record was rewritten: %+v", got)
	}
}

func TestAttemptValidateRefusesRecordsThatCannotBeFaithful(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Attempt){
		"number":                   func(a *Attempt) { a.Number = 0 },
		"no parent":                func(a *Attempt) { a.Parent = " " },
		"unknown outcome":          func(a *Attempt) { a.Outcome = "pending" },
		"discard without a cause":  func(a *Attempt) { *a = discardedAttempt(1); a.Cause = "" },
		"kept without scores":      func(a *Attempt) { a.Scores = nil },
		"kept without a commit":    func(a *Attempt) { a.Commit = "" },
		"kept with an invalid run": func(a *Attempt) { a.Measurement.Valid = false },
		"kept with a cause":        func(a *Attempt) { a.Cause = CauseNoImprovement },
	} {
		a := keptAttempt(1)
		mutate(&a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := keptAttempt(1).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := discardedAttempt(1).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := RecordAttempt(context.Background(), nil, fixturekit.Stream, Attempt{}); err == nil {
		t.Fatal("an empty attempt was recorded")
	}
}
