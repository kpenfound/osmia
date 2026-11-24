package executionjourney

import (
	context "context"
	fmt "fmt"
	agent "github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	strings "strings"
	sync "sync"
	testing "testing"
)

func TestMasonInfrastructureFailureContestsUnitAndStreakResets(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.stop(t)
	fake.Play[service.TestMasonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrCrashTurn }
	owner := intake.MasonAgent("resume") + "-owner-revise-1"
	fake.Play[owner] = intakefixture.ReportDone("Owner revision")
	f.engine.Mu.Lock()
	f.engine.Turns[owner] = fake.Turn
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "crashing-mason", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitContested)

	q := f.awaitCompleted(t, stream, intake.MasonAgent("resume"), service.TestMasonTurnID("resume"))
	checkExhausted(t, q)
	ran := 0
	for _, req := range fake.Requests(stream) {
		if req.Name == service.TestMasonTurnID("resume") {
			ran++
		}
	}
	if ran != service.TestTurnRetries+1 {
		t.Fatalf("the crashing mason turn ran %d times", ran)
	}
	checkFailureContest(t, f, stream, "resume", intake.UnitImplementing, q)
	if s, ok := f.streak(t, intake.MasonRole); !ok || s.Profile != "default" || s.Consecutive != service.TestTurnRetries+1 || s.LastFailure != q.Attempts[service.TestTurnRetries].Failure || !s.LastAt.Equal(q.Attempts[service.TestTurnRetries].At) {
		t.Fatalf("mason streak %+v %t", s, ok)
	}

	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "The runner is fixed"); err != nil {
		t.Fatal(err)
	}
	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
	if s, ok := f.streak(t, intake.MasonRole); ok {
		t.Fatalf("mason streak after a success: %+v", s)
	}
	if th := f.thread(t, stream, intake.MasonAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Request.TurnID != owner || th.Turns[1].Status() != "idle" {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	fake.Check(t)
}

func TestMasonBehaviouralFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.stop(t)
	fake.Play[service.TestMasonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrFailTurn }
	continued := intake.MasonAgent("resume") + "-continue-1"
	fake.Play[continued] = intakefixture.ReportDone("Built")
	f.engine.Mu.Lock()
	f.engine.Turns[continued] = fake.Turn
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "misreporting-mason", fixturekit.ValidPlan)
	q := f.awaitCompleted(t, stream, intake.MasonAgent("resume"), service.TestMasonTurnID("resume"))
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Behavioural || len(q.Attempts) != 1 {
		t.Fatalf("turn ended %s with %+v after %d attempts", q.Status(), q.Response, len(q.Attempts))
	}

	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
	ran := map[string]int{}
	for _, req := range fake.Requests(stream) {
		ran[req.Name]++
	}
	if ran[service.TestMasonTurnID("resume")] != 1 || ran[continued] != 1 || len(ran) != 2 {
		t.Fatalf("mason turns %v", ran)
	}
	if th := f.thread(t, stream, intake.MasonAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Request.TurnID != continued {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	if s, ok := f.streak(t, intake.MasonRole); ok {
		t.Fatalf("a behavioural failure made a streak: %+v", s)
	}
	fake.Check(t)
}

func TestReviewerInfrastructureFailureContestsUnit(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.stop(t)
	fake.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Built")
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	var mu sync.Mutex
	crash, reviews := true, map[string]int{}
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			return chief.Turn(ctx, req, verified, tools)
		}
		mu.Lock()
		reviews[req.Name]++
		crashing := crash
		mu.Unlock()
		if crashing {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Crashed", SessionDir: req.SessionDir, NumTurns: 1, ExitCode: 1}, nil
		}
		body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("verdict %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "crashing-reviewer", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitContested)
	th := f.thread(t, stream, intake.ReviewerAgent("resume"))
	q := f.awaitCompleted(t, stream, intake.ReviewerAgent("resume"), th.Turns[len(th.Turns)-1].Request.TurnID)
	checkExhausted(t, q)
	checkFailureContest(t, f, stream, "resume", intake.UnitReviewing, q)
	mu.Lock()
	if reviews[q.Request.TurnID] != service.TestTurnRetries+1 {
		t.Fatalf("review attempts %v", reviews)
	}
	crash = false
	mu.Unlock()
	if s, ok := f.streak(t, intake.ReviewerRole); !ok || s.Consecutive != service.TestTurnRetries+1 {
		t.Fatalf("reviewer streak %+v %t", s, ok)
	}

	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "Rework it"); err == nil || !strings.Contains(err.Error(), "use review") {
		t.Fatalf("revise on a failed review: %v", err)
	}
	ruling, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "The reviewer is back")
	if err != nil || ruling.Ruling.Decision != "review" || ruling.Ruling.Contest != execution.FailureContestID(q.Response.ID) {
		t.Fatalf("review ruling %+v %v", ruling, err)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Again"); err == nil {
		t.Fatal("accepted a second ruling")
	}
	intakefixture.Soon(t, "the unit to be reviewed again", func() bool {
		_, err := approvedReviewResult(f, stream, "resume")
		return err == nil
	})
	th = f.thread(t, stream, intake.ReviewerAgent("resume"))
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID == q.Request.TurnID || th.Turns[1].Status() != "idle" {
		t.Fatalf("reviewer turns %+v", th.Turns)
	}
	if s, ok := f.streak(t, intake.ReviewerRole); ok {
		t.Fatalf("reviewer streak after a success: %+v", s)
	}
	fake.Check(t)
}

func TestFailureHistoryCollapsesRepeatedAttempts(t *testing.T) {
	t.Parallel()
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
		if got := execution.FailureHistory(trace.QueuedTurn{Attempts: c.attempts}); got != c.want {
			t.Errorf("failureHistory\n%s\nwant\n%s", got, c.want)
		}
	}
}

func (f *shedFixture) streak(t *testing.T, role string) (api.FailureStreak, bool) {
	t.Helper()
	all, err := f.c.Statuses(context.Background())
	fixturekit.Must(t, err)
	for _, s := range all.FailureStreaks {
		if s.Role == role {
			return s, true
		}
	}
	return api.FailureStreak{}, false
}

func checkExhausted(t *testing.T, q trace.QueuedTurn) {
	t.Helper()
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Infrastructure || len(q.Attempts) != service.TestTurnRetries+1 {
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
	fixturekit.Must(t, err)
	var reason string
	for _, u := range status.Units {
		if u.Unit == unit {
			reason = u.Reason
		}
	}
	for _, g := range status.Gates {
		if g.Kind == intake.UnitContested && g.Reference == unit {
			t.Fatal("engineering failure automatically required owner attention")
		}
	}
	for _, want := range []string{"failed on every retry and fallback profile", q.Request.TurnID, fmt.Sprintf("%d on profile default: infrastructure failure", service.TestTurnRetries+1)} {
		if !strings.Contains(reason, want) {
			t.Fatalf("contested status %+v lacks %q", status.Units, want)
		}
	}
	id := execution.FailureContestID(q.Response.ID)
	var contest trace.Transition
	for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.ID == id {
			contest = tr
		}
	}
	if contest.From != from || contest.To != intake.UnitContested || contest.Cause != q.Response.ID || contest.Reason != reason {
		t.Fatalf("contest transition %+v", contest)
	}
	outbox, err := f.repository().Outbox(stream)
	fixturekit.Must(t, err)
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

func approvedReviewResult(f *shedFixture, stream config.WorkstreamID, unit string) (service.UnitReviewResult, error) {
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
