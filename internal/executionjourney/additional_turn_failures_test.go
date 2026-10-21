package executionjourney

import (
	context "context"
	fmt "fmt"
	strings "strings"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func (f *shedFixture) streak(t *testing.T, role string) (FailureStreak, bool) {
	t.Helper()
	all, err := f.c.Statuses(context.Background())
	must(t, err)
	for _, s := range all.FailureStreaks {
		if s.Role == role {
			return s, true
		}
	}
	return FailureStreak{}, false
}

func checkExhausted(t *testing.T, q trace.QueuedTurn) {
	t.Helper()
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Infrastructure || len(q.Attempts) != turnRetries+1 {
		t.Fatalf("turn %s ended %s with %+v after %d attempts", q.Request.TurnID, q.Status(), q.Response, len(q.Attempts))
	}
	for _, a := range q.Attempts {
		if a.FailureClass != coreadapter.Infrastructure || a.Profile.Name != "default" || a.Failure == "" {
			t.Fatalf("attempt %+v", a)
		}
	}
}

func checkFailureContest(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit, from string, q trace.QueuedTurn) {
	t.Helper()
	status, err := f.c.Status(context.Background(), stream)
	must(t, err)
	var reason string
	for _, u := range status.Units {
		if u.Unit == unit {
			reason = u.Reason
		}
	}
	for _, g := range status.Gates {
		if g.Kind == UnitContested && g.Reference == unit {
			t.Fatal("engineering failure automatically required owner attention")
		}
	}
	for _, want := range []string{"failed on every retry and fallback profile", q.Request.TurnID, fmt.Sprintf("%d on profile default: infrastructure failure", turnRetries+1)} {
		if !strings.Contains(reason, want) {
			t.Fatalf("contested status %+v lacks %q", status.Units, want)
		}
	}
	id := failureContestID(q.Response.ID)
	var contest trace.Transition
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.ID == id {
			contest = tr
		}
	}
	if contest.From != from || contest.To != UnitContested || contest.Cause != q.Response.ID || contest.Reason != reason {
		t.Fatalf("contest transition %+v", contest)
	}
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	notices := 0
	for _, e := range outbox {
		if e.Event.ID == trace.EventID(id, "unit") && e.Event.Body == reason {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("contest notices %d in %+v", notices, outbox)
	}
}

func approvedReviewResult(f *shedFixture, stream config.WorkstreamID, unit string) (UnitReviewResult, error) {
	r := &reviewers{masons: &masons{repository: f.repository()}}
	result, ok, err := r.storedResult(stream, unit, trace.WorkflowState{})
	if err != nil {
		return result, err
	}
	if !ok || result.Verdict.Decision != "satisfactory" {
		return result, fmt.Errorf("no satisfactory review")
	}
	return result, nil
}
