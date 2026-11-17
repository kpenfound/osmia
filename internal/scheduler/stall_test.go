package scheduler

import (
	"testing"
	"time"
)

const (
	testAfter = 45 * time.Minute
	testLimit = 5
)

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// quietFor returns a subject quiet for d.
func quietFor(d time.Duration) Quiet { return Quiet{Since: testNow.Add(-d)} }

func TestAssessRaisesAStallForAQuietSubjectWithNoExplanation(t *testing.T) {
	t.Parallel()
	got := Assess(quietFor(testAfter), testNow, testAfter, testLimit)
	if !got.Stalled || got.Explanation != NotExplained || got.Quiet != testAfter {
		t.Fatalf("a subject quiet for the whole window = %+v", got)
	}
	if got := Assess(quietFor(testAfter-time.Second), testNow, testAfter, testLimit); got.Stalled {
		t.Fatalf("a subject quiet for less than the window stalled: %+v", got)
	}
}

func TestAssessIsOffWithAWindowOfZero(t *testing.T) {
	t.Parallel()
	if got := Assess(quietFor(1000*time.Hour), testNow, 0, testLimit); got.Stalled {
		t.Fatalf("the watchdog is off but a subject stalled: %+v", got)
	}
	if got := Assess(Quiet{}, testNow, testAfter, testLimit); got.Stalled {
		t.Fatalf("a subject that never progressed has no clock: %+v", got)
	}
}

// Each listed explanation suppresses the stall, and names itself.
func TestAssessSuppressesAStallWhenSomethingExplainsTheQuiet(t *testing.T) {
	t.Parallel()
	inFlight := Operation{Action: "land", LastAttempt: testNow.Add(-time.Minute), Failures: 2}
	for _, tc := range []struct {
		name string
		edit func(*Quiet)
		want Explanation
	}{
		{"open owner card", func(q *Quiet) { q.Card = true }, ExplainedByCard},
		{"pause", func(q *Quiet) { q.Paused = true }, ExplainedByPause},
		{"running or queued turn", func(q *Quiet) { q.Turn = true }, ExplainedByTurn},
		{"live dependency wait", func(q *Quiet) { q.Waiting = true }, ExplainedByDependency},
		{"operation in flight", func(q *Quiet) { q.Operations = []Operation{inFlight} }, ExplainedByOperation},
		{"hold of a pause", func(q *Quiet) { q.Operations = []Operation{{Action: "land", Held: HeldByPause}} }, ExplainedByPause},
		{"hold of an owner card", func(q *Quiet) { q.Operations = []Operation{{Action: "land", Held: HeldByCard}} }, ExplainedByCard},
		{"hold of a live dependency wait", func(q *Quiet) { q.Operations = []Operation{{Action: "land", Held: HeldByDependency}} }, ExplainedByDependency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := quietFor(2 * testAfter)
			tc.edit(&q)
			got := Assess(q, testNow, testAfter, testLimit)
			if got.Stalled || got.Explanation != tc.want {
				t.Fatalf("assessed %+v, want %q and no stall", got, tc.want)
			}
		})
	}
}

// An operation explains the quiet only while it can still reach a result: its
// latest attempt is within the window and its failures are below the limit.
func TestAssessDoesNotLetAnOperationThatCannotEndExplainTheQuiet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		op   Operation
	}{
		{"latest attempt older than the window", Operation{Action: "land", LastAttempt: testNow.Add(-testAfter), Failures: 1, LastFailure: "Clone lock contention: held for 15m"}},
		{"never attempted", Operation{Action: "publish"}},
		{"failures reached the limit", Operation{Action: "rebase", LastAttempt: testNow.Add(-time.Minute), Failures: testLimit, LastFailure: "Clone recovery failure: restore timed out"}},
		{"failures past the limit", Operation{Action: "rebase", LastAttempt: testNow.Add(-time.Minute), Failures: testLimit + 3, LastFailure: "git: remote hung up"}},
		{"held for another reason", Operation{Action: "publish", LastAttempt: testNow.Add(-time.Minute), Held: HeldByOther, LastFailure: "no credential"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := quietFor(2 * testAfter)
			q.Operations = []Operation{tc.op}
			got := Assess(q, testNow, testAfter, testLimit)
			if !got.Stalled || len(got.Unresolved) != 1 || got.Unresolved[0] != tc.op {
				t.Fatalf("assessed %+v: the operation must not suppress the stall and must be carried as evidence", got)
			}
		})
	}
	// Just inside the window and below the limit it still explains the quiet.
	q := quietFor(2 * testAfter)
	q.Operations = []Operation{{Action: "land", LastAttempt: testNow.Add(-testAfter + time.Second), Failures: testLimit - 1}}
	if got := Assess(q, testNow, testAfter, testLimit); got.Stalled || got.Explanation != ExplainedByOperation {
		t.Fatalf("an operation inside the window and below the limit = %+v", got)
	}
}

// One operation that explains the quiet suppresses it, even beside one that
// cannot end.
func TestAssessSuppressesAStallWhileAnotherOperationIsInFlight(t *testing.T) {
	t.Parallel()
	q := quietFor(2 * testAfter)
	q.Operations = []Operation{
		{Action: "rebase", LastAttempt: testNow.Add(-time.Hour), Failures: testLimit},
		{Action: "check", LastAttempt: testNow.Add(-time.Minute)},
	}
	if got := Assess(q, testNow, testAfter, testLimit); got.Stalled || got.Explanation != ExplainedByOperation {
		t.Fatalf("assessed %+v", got)
	}
}

// A quiet subject that has explanations only inside its window is stalled once
// the clock passes it: the window is the subject's own, not the explanation's.
func TestAssessCountsTheQuietFromTheLastProgress(t *testing.T) {
	t.Parallel()
	q := Quiet{Since: testNow.Add(-testAfter / 2)}
	if got := Assess(q, testNow, testAfter, testLimit); got.Stalled || got.Quiet != testAfter/2 {
		t.Fatalf("assessed %+v", got)
	}
	if got := Assess(q, testNow.Add(testAfter/2), testAfter, testLimit); !got.Stalled {
		t.Fatalf("a subject quiet for the whole window did not stall: %+v", got)
	}
}
