package executionjourney

import (
	context "context"
	fmt "fmt"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMasonInfrastructureFailureContestsUnitAndStreakResets(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	fake.Play[masonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return errCrashTurn }
	owner := masonAgent("resume") + "-owner-revise-1"
	fake.Play[owner] = reportDone("Owner revision")
	f.engine.Mu.Lock()
	f.engine.Turns[owner] = fake.Turn
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "crashing-mason", validPlan)
	f.awaitUnit(t, stream, "resume", UnitContested)

	q := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
	checkExhausted(t, q)
	ran := 0
	for _, req := range fake.Requests(stream) {
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
	fake.Check(t)
}

func TestMasonBehaviouralFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	var passed atomic.Int64
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", validPlan, "", func(opts *Options) {
		opts.schedulePassed = func() { passed.Add(1) }
	})
	defer f.stop(t)
	fake.Play[masonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return errFailTurn }
	stream := f.seedBuilding(t, "misreporting-mason", validPlan)
	q := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Behavioural || len(q.Attempts) != 1 {
		t.Fatalf("turn ended %s with %+v after %d attempts", q.Status(), q.Response, len(q.Attempts))
	}
	// Later passes leave the unit implementing, and never contest it.
	awaitPasses(t, &passed, 3)
	if state, err := f.repository().Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitImplementing {
		t.Fatalf("unit resume is %+v: %v", state, err)
	}
	if len(fake.Requests(stream)) != 1 {
		t.Fatalf("mason turns %+v", fake.Requests(stream))
	}
	if s, ok := f.streak(t, masonRole); ok {
		t.Fatalf("a behavioural failure made a streak: %+v", s)
	}
	fake.Check(t)
}

func TestReviewerInfrastructureFailureContestsUnit(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	fake.Play[masonTurnID("resume")] = reportDone("Built")
	chief := &chief{P: &faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	var mu sync.Mutex
	crash, reviews := true, map[string]int{}
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			return chief.Turn(ctx, req, verified, tools)
		}
		mu.Lock()
		reviews[req.Name]++
		crashing := crash
		mu.Unlock()
		if crashing {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Crashed", SessionDir: req.SessionDir, NumTurns: 1, ExitCode: 1}, nil
		}
		body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("verdict %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "crashing-reviewer", validPlan)
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
	fake.Check(t)
}

func TestFailureHistoryCollapsesRepeatedAttempts(t *testing.T) {
	attempt := func(n int, profile, failure string) trace.TurnAttempt {
		return trace.TurnAttempt{Number: n, Profile: coreadapter.Profile{Name: profile}, FailureClass: coreadapter.Infrastructure, Failure: failure}
	}
	for _, c := range []struct {
		attempts []trace.TurnAttempt
		want     string
	}{
		{[]trace.TurnAttempt{attempt(1, "fast", "timed out")}, "attempt 1 on profile fast: infrastructure failure: timed out"},
		{[]trace.TurnAttempt{attempt(1, "fast", "timed out"), attempt(2, "fast", "timed out"), attempt(3, "fast", "timed out")},
			"attempts 1 to 3 on profile fast: infrastructure failure: timed out"},
		{[]trace.TurnAttempt{attempt(1, "fast", "timed out"), attempt(2, "fast", "timed out"), attempt(3, "fast", "usage limit"), attempt(4, "slow", "usage limit")},
			"attempts 1 to 2 on profile fast: infrastructure failure: timed out; attempt 3 on profile fast: infrastructure failure: usage limit; attempt 4 on profile slow: infrastructure failure: usage limit"},
	} {
		if got := failureHistory(trace.QueuedTurn{Attempts: c.attempts}); got != c.want {
			t.Errorf("failureHistory\n%s\nwant\n%s", got, c.want)
		}
	}
}
