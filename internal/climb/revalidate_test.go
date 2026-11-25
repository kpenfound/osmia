package climb

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func (r *loopRig) terminalRecords(n int) int {
	r.t.Helper()
	docs, err := trace.Read[trace.Document](r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	records := 0
	for _, d := range docs {
		if d.Path == trace.ClimbAttemptPath(n) {
			records++
		}
	}
	return records
}

func lowerRSSLimit(s *Spec) { s.Guardrails.Limits[0].Max = ptr(100.0) }

func addGuardrailCheck(s *Spec) { s.Guardrails.Checks = append(s.Guardrails.Checks, "go:test") }

func TestAKeptAttemptThatBreaksAnAmendedLimitOrCheckIsDiscardedWhateverTheReviewerAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		amend  func(*Spec)
		script script
		cause  Cause
		reason string
	}{
		{"a lowered secondary maximum", lowerRSSLimit, script{}, CauseLimits, "peak_rss_mb is 120, above its limit 100"},
		{"an added guardrail check the candidate fails", addGuardrailCheck, script{failChecks: true}, CauseGuardrails, "go:test failed"},
	} {
		for _, pending := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, review pending=%v", tc.name, pending), func(t *testing.T) {
				r := newLoopRig(t, func(s *Spec) {
					s.Stop.MaxAttempts = 2
					s.Guardrails.Limits[0].Max = ptr(150.0)
				})
				tc.script.changed, tc.script.report, tc.script.pendingReview = edits("src/work.go"), good("cache the parse"), pending
				r.host.script(1, tc.script)
				r.host.script(2, script{changed: edits("src/work.go"), report: good("second idea")})
				r.score(1, 90, 120)
				r.score(2, 80, 90)
				want := PhaseMeasured
				if pending {
					r.reconcile()
				} else {
					want = PhaseReviewed
					r.crash("climb:reviewed")
				}
				if got := r.phase(1); got != string(want) {
					t.Fatalf("attempt 1 is %q, want %q: kept under the old limits, with the review pending or recorded", got, want)
				}
				if len(r.host.guarded) != 0 {
					t.Fatalf("checks ran before the amendment: %v", r.host.guarded)
				}
				r.amend(tc.amend)
				r.reconcile()

				a := r.attempt(1)
				if a.Outcome != OutcomeDiscarded || a.Cause != tc.cause || !strings.Contains(a.Reason, tc.reason) || !strings.Contains(a.Reason, "amendment") || a.Commit != "" {
					t.Fatalf("attempt 1 %+v", a)
				}
				if a.Scores == nil || a.Scores.Primary != 90 || a.Change == nil || *a.Change != -10 {
					t.Fatalf("the discarded attempt keeps what was measured: %+v", a)
				}
				a2 := r.attempt(2)
				if len(r.host.moves) != 1 || len(r.host.squashes) != 1 || strings.Contains(r.host.squashes[0], "cand0001") || r.tip() != a2.Commit {
					t.Fatalf("the discarded candidate reached the climb branch: moves %v, squashes %v, tip %s; only attempt 2 lands as %s", r.host.moves, r.host.squashes, r.tip(), a2.Commit)
				}
				if r.terminalRecords(1) != 1 || r.phase(1) != string(PhaseCleaned) {
					t.Fatalf("attempt 1 has %d terminal records and is %q", r.terminalRecords(1), r.phase(1))
				}
				if r.host.branches[a.Branch] != "cand0001" {
					t.Fatalf("the discarded branch holds %q", r.host.branches[a.Branch])
				}
				if a2.Outcome != OutcomeKept || a2.Parent != "base0001" || a2.Best != 100 {
					t.Fatalf("attempt 2 %+v starts from the committed best", a2)
				}
				if got := len(r.attempts()); got != 2 {
					t.Fatalf("%d recorded attempts", got)
				}
			})
		}
	}
}

func TestARestartBetweenTheAmendmentAndTheReviewersVerdictEndsTheAttemptOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		amend func(*Spec)
		fail  bool
		hook  string
	}{
		{"a lowered maximum, crashed after the discard is recorded", lowerRSSLimit, false, "climb:recorded"},
		{"an added failing check, crashed while the workspace is dropped", addGuardrailCheck, true, "climb:dropping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t, func(s *Spec) {
				s.Stop.MaxAttempts = 2
				s.Guardrails.Limits[0].Max = ptr(150.0)
			})
			r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse"), pendingReview: true, failChecks: tc.fail})
			r.host.script(2, script{changed: edits("src/work.go"), report: good("second idea")})
			r.score(1, 90, 120)
			r.score(2, 80, 90)
			r.reconcile()
			r.amend(tc.amend)
			r.crash(tc.hook)
			r.reconcile()

			if a := r.attempt(1); a.Outcome != OutcomeDiscarded {
				t.Fatalf("attempt 1 %+v", a)
			}
			if r.terminalRecords(1) != 1 || r.terminalRecords(2) != 1 || len(r.attempts()) != 2 {
				t.Fatalf("records %d and %d of %d attempts", r.terminalRecords(1), r.terminalRecords(2), len(r.attempts()))
			}
			if len(r.host.squashes) != 1 || len(r.host.moves) != 1 || r.tip() != r.attempt(2).Commit {
				t.Fatalf("squashes %v, moves %v, tip %s: only attempt 2 lands", r.host.squashes, r.host.moves, r.tip())
			}
			if len(r.host.threads) != 2 || len(r.host.workspaces) != 0 {
				t.Fatalf("threads %v, workspaces %v", r.host.threads, r.host.workspaces)
			}
		})
	}
}

func TestAnAmendedLimitIsAlsoAppliedToAnApprovedReviewThatWasRecordedBeforeIt(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Guardrails.Limits[0].Max = ptr(150.0) })
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
	r.score(1, 90, 120)
	r.crash("climb:reviewed")
	if got := r.phase(1); got != string(PhaseReviewed) {
		t.Fatalf("attempt 1 is %q", got)
	}
	r.amend(lowerRSSLimit)
	r.reconcile()
	if a := r.attempt(1); a.Outcome != OutcomeDiscarded || a.Cause != CauseLimits {
		t.Fatalf("attempt 1 %+v: the approval cannot promote it", a)
	}
	if len(r.host.squashes) != 0 || len(r.host.moves) != 0 || r.terminalRecords(1) != 1 {
		t.Fatalf("squashes %v, moves %v, %d records", r.host.squashes, r.host.moves, r.terminalRecords(1))
	}
}

func TestAnAmendmentThatKeepsTheCandidateRunsTheNewChecksOnceAndPromotesIt(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) {
		s.Stop.MaxAttempts = 1
		s.Guardrails.Limits[0].Max = ptr(150.0)
	})
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse"), pendingReview: true})
	r.score(1, 90, 120)
	r.reconcile()
	r.amend(func(s *Spec) { addGuardrailCheck(s); s.Guardrails.Limits[0].Max = ptr(125.0) })
	r.reconcile()
	r.reconcile()
	if !slices.Equal(r.host.guarded, []int{1}) {
		t.Fatalf("the added check ran for attempts %v, want once for attempt 1", r.host.guarded)
	}
	a := r.attempt(1)
	if a.Outcome != OutcomeKept || r.tip() != a.Commit || len(r.host.moves) != 1 || r.terminalRecords(1) != 1 {
		t.Fatalf("attempt 1 %+v, tip %s, moves %v", a, r.tip(), r.host.moves)
	}
	j, _, err := ReadJournal(r.repo, fixturekit.Stream, 1)
	fixturekit.Must(t, err)
	if j.DecidedRevision != 2 || j.Guardrails == nil || !j.Guardrails.Green {
		t.Fatalf("journal %+v", j)
	}
}

func TestAnAmendmentThatLoosensALimitOrChangesOnlyTheGoalOrStopRulesLeavesTheAttemptUndisturbed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		amend func(*Spec)
	}{
		{"a loosened maximum", func(s *Spec) { s.Guardrails.Limits[0].Max = ptr(500.0) }},
		{"the goal", func(s *Spec) { s.Goal = "Make the parser much faster on the corpus." }},
		{"the stop rules", func(s *Spec) { s.Stop.MaxAttempts = 7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t, func(s *Spec) {
				s.Stop.MaxAttempts = 1
				s.Guardrails.Limits[0].Max = ptr(150.0)
				s.Guardrails.Checks = []string{"go:test"}
			})
			r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse"), pendingReview: true})
			r.score(1, 90, 120)
			r.reconcile()
			r.amend(tc.amend)
			r.reconcile()
			a := r.attempt(1)
			if a.Outcome != OutcomeKept || r.tip() != a.Commit || len(r.host.moves) != 1 || r.terminalRecords(1) != 1 {
				t.Fatalf("attempt 1 %+v, tip %s, moves %v", a, r.tip(), r.host.moves)
			}
			if !slices.Equal(r.host.guarded, []int{1}) {
				t.Fatalf("checks ran for attempts %v, want only the one before the amendment", r.host.guarded)
			}
		})
	}
}

func TestAPromotionAlreadyUnderWayCompletesAfterAnAmendmentThatWouldDiscardTheCandidate(t *testing.T) {
	t.Parallel()
	for _, hook := range []string{"climb:promoting", "climb:moved", "climb:promoted"} {
		t.Run(hook, func(t *testing.T) {
			r := newLoopRig(t, func(s *Spec) {
				s.Stop.MaxAttempts = 1
				s.Guardrails.Limits[0].Max = ptr(150.0)
			})
			r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
			r.score(1, 90, 120)
			r.crash(hook)
			r.amend(func(s *Spec) { lowerRSSLimit(s); addGuardrailCheck(s) })
			r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse"), failChecks: true})
			r.reconcile()
			a := r.attempt(1)
			if a.Outcome != OutcomeKept || r.tip() != a.Commit || r.terminalRecords(1) != 1 || len(r.host.moves) != 1 || len(r.host.guarded) != 0 {
				t.Fatalf("attempt 1 %+v, tip %s, moves %v, checks %v", a, r.tip(), r.host.moves, r.host.guarded)
			}
		})
	}
}
