package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// streak returns the fixture's infrastructure failure streak of role, if any.
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

// awaitCompleted waits until the thread's turn has completed and returns it.
func (f *shedFixture) awaitCompleted(t *testing.T, stream config.WorkstreamID, agent, turn string) trace.QueuedTurn {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().Thread(stream, agent)
		if err == nil {
			for _, q := range th.Turns {
				if q.Request.TurnID == turn && !q.CompletedAt.IsZero() {
					return q
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s of %s never completed: %+v %v", turn, agent, th.Turns, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkExhausted checks that q failed with an infrastructure failure on each
// of its three attempts on the fixture's default profile.
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

// checkFailureContest checks the unit's contested gate, and the transition
// and notice that raised it, against its failed turn q.
func checkFailureContest(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit, from string, q trace.QueuedTurn) {
	t.Helper()
	status, err := f.c.Status(context.Background(), stream)
	must(t, err)
	var gate trace.OwnerGate
	for _, g := range status.Gates {
		if g.Kind == UnitContested && g.Reference == unit {
			gate = g
		}
	}
	for _, want := range []string{"failed on every retry and fallback profile", q.Request.TurnID, fmt.Sprintf("attempt %d on profile default: infrastructure failure", turnRetries+1)} {
		if !strings.Contains(gate.Reason, want) {
			t.Fatalf("contested gate %+v lacks %q", status.Gates, want)
		}
	}
	id := failureContestID(q.Response.ID)
	var contest trace.Transition
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.ID == id {
			contest = tr
		}
	}
	if contest.From != from || contest.To != UnitContested || contest.Cause != q.Response.ID || contest.Reason != gate.Reason {
		t.Fatalf("contest transition %+v", contest)
	}
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	notices := 0
	for _, e := range outbox {
		if e.Event.ID == trace.EventID(id, "unit") && e.Event.Body == gate.Reason {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("contest notices %d in %+v", notices, outbox)
	}
}

func TestMasonInfrastructureFailureContestsUnitAndStreakResets(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	fake.play[masonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return errCrashTurn }
	owner := masonAgent("resume") + "-owner-revise-1"
	fake.play[owner] = reportDone("Owner revision")
	f.engine.mu.Lock()
	f.engine.turns[owner] = fake.turn
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "crashing-mason")
	f.awaitUnit(t, stream, "resume", UnitContested)

	q := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
	checkExhausted(t, q)
	ran := 0
	for _, req := range fake.requests(stream) {
		if req.Name == masonTurnID("resume") {
			ran++
		}
	}
	if ran != turnRetries+1 {
		t.Fatalf("the crashing mason turn ran %d times", ran)
	}
	checkFailureContest(t, f, stream, "resume", UnitImplementing, q)
	if s, ok := f.streak(t, masonRole); !ok || s.Profile != "default" || s.Consecutive != turnRetries+1 || s.LastFailure != q.Attempts[turnRetries].Failure || !s.LastAt.Equal(q.Attempts[turnRetries].At) {
		t.Fatalf("mason streak %+v %t", s, ok)
	}

	// The owner's revise ruling runs the next mason turn, whose success resets
	// the streak.
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "The runner is fixed"); err != nil {
		t.Fatal(err)
	}
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	if s, ok := f.streak(t, masonRole); ok {
		t.Fatalf("mason streak after a success: %+v", s)
	}
	if th := f.thread(t, stream, masonAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Request.TurnID != owner || th.Turns[1].Status() != "idle" {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	fake.check(t)
}

func TestMasonBehaviouralFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	fake.play[masonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return errFailTurn }
	stream, _ := f.builtAs(t, "misreporting-mason")
	q := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Behavioural || len(q.Attempts) != 1 {
		t.Fatalf("turn ended %s with %+v after %d attempts", q.Status(), q.Response, len(q.Attempts))
	}
	// Later passes leave the unit implementing, and never contest it.
	time.Sleep(2 * time.Second)
	if state, err := f.repository().Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitImplementing {
		t.Fatalf("unit resume is %+v: %v", state, err)
	}
	if len(fake.requests(stream)) != 1 {
		t.Fatalf("mason turns %+v", fake.requests(stream))
	}
	if s, ok := f.streak(t, masonRole); ok {
		t.Fatalf("a behavioural failure made a streak: %+v", s)
	}
	fake.check(t)
}

func TestReviewerInfrastructureFailureContestsUnit(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	fake.play[masonTurnID("resume")] = reportDone("Built")
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	var mu sync.Mutex
	crash, reviews := true, map[string]int{}
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			return chief.turn(ctx, req, verified, tools)
		}
		mu.Lock()
		reviews[req.Name]++
		crashing := crash
		mu.Unlock()
		if crashing {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Crashed", SessionDir: req.SessionDir, NumTurns: 1, ExitCode: 1}, nil
		}
		body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": reviewEvidence(), "findings": []ReviewFinding{}})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("verdict %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "crashing-reviewer")
	f.awaitUnit(t, stream, "resume", UnitContested)
	th := f.thread(t, stream, reviewerAgent("resume"))
	q := f.awaitCompleted(t, stream, reviewerAgent("resume"), th.Turns[len(th.Turns)-1].Request.TurnID)
	checkExhausted(t, q)
	checkFailureContest(t, f, stream, "resume", UnitReviewing, q)
	mu.Lock()
	if reviews[q.Request.TurnID] != turnRetries+1 {
		t.Fatalf("review attempts %v", reviews)
	}
	crash = false
	mu.Unlock()
	if s, ok := f.streak(t, reviewerRole); !ok || s.Consecutive != turnRetries+1 {
		t.Fatalf("reviewer streak %+v %t", s, ok)
	}

	// A failed review left no findings: the owner can only have it reviewed
	// again, by a new review turn over the same candidate.
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "Rework it"); err == nil || !strings.Contains(err.Error(), "use review") {
		t.Fatalf("revise on a failed review: %v", err)
	}
	ruling, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "The reviewer is back")
	if err != nil || ruling.Ruling.Decision != "review" || ruling.Ruling.Contest != failureContestID(q.Response.ID) {
		t.Fatalf("review ruling %+v %v", ruling, err)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Again"); err == nil {
		t.Fatal("accepted a second ruling")
	}
	deadline := time.Now().Add(demoTimeout)
	for {
		if _, err := approvedReviewResult(f, stream, "resume"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unit was not reviewed again: %+v", f.thread(t, stream, reviewerAgent("resume")).Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
	th = f.thread(t, stream, reviewerAgent("resume"))
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID == q.Request.TurnID || th.Turns[1].Status() != "idle" {
		t.Fatalf("reviewer turns %+v", th.Turns)
	}
	if s, ok := f.streak(t, reviewerRole); ok {
		t.Fatalf("reviewer streak after a success: %+v", s)
	}
	fake.check(t)
}

// approvedReviewResult returns the unit's satisfactory review result once it
// is recorded.
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
