package executionjourney

import (
	context "context"
	fmt "fmt"
	strings "strings"
	sync "sync"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMasonInfrastructureFailureContestsUnitAndStreakResets(t *testing.T) {
	t.Parallel()
	f, fake := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.Stop(t)
	fake.Play[execution.MasonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrCrashTurn }
	owner := intake.MasonAgent("resume") + "-owner-revise-1"
	fake.Play[owner] = intakefixture.ReportDone("Owner revision")
	f.Engine.Mu.Lock()
	f.Engine.Turns[owner] = fake.Turn
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "crashing-mason", fixturekit.ValidPlan)
	f.AwaitUnit(t, stream, "resume", intake.UnitContested)

	q := awaitCompleted(f, t, stream, intake.MasonAgent("resume"), execution.MasonTurnID("resume"))
	checkExhausted(t, q)
	ran := 0
	for _, req := range fake.Requests(stream) {
		if req.Name == execution.MasonTurnID("resume") {
			ran++
		}
	}
	if ran != service.TurnRetries+1 {
		t.Fatalf("the crashing mason turn ran %d times", ran)
	}
	checkFailureContest(t, f, stream, "resume", intake.UnitImplementing, q)
	if s, ok := streak(f, t, intake.MasonRole); !ok || s.Profile != "default" || s.Consecutive != service.TurnRetries+1 || s.LastFailure != q.Attempts[service.TurnRetries].Failure || !s.LastAt.Equal(q.Attempts[service.TurnRetries].At) {
		t.Fatalf("mason streak %+v %t", s, ok)
	}

	if _, err := f.C.RuleContested(context.Background(), stream, "resume", "revise", "The runner is fixed"); err != nil {
		t.Fatal(err)
	}
	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)
	if s, ok := streak(f, t, intake.MasonRole); ok {
		t.Fatalf("mason streak after a success: %+v", s)
	}
	if th := f.Thread(t, stream, intake.MasonAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Request.TurnID != owner || th.Turns[1].Status() != "idle" {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	fake.Check(t)
}

func TestMasonBehaviouralFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	f, fake := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.Stop(t)
	fake.Play[execution.MasonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrFailTurn }
	continued := intake.MasonAgent("resume") + "-continue-1"
	fake.Play[continued] = intakefixture.ReportDone("Built")
	f.Engine.Mu.Lock()
	f.Engine.Turns[continued] = fake.Turn
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "misreporting-mason", fixturekit.ValidPlan)
	q := awaitCompleted(f, t, stream, intake.MasonAgent("resume"), execution.MasonTurnID("resume"))
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Behavioural || len(q.Attempts) != 1 {
		t.Fatalf("turn ended %s with %+v after %d attempts", q.Status(), q.Response, len(q.Attempts))
	}

	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)
	ran := map[string]int{}
	for _, req := range fake.Requests(stream) {
		ran[req.Name]++
	}
	if ran[execution.MasonTurnID("resume")] != 1 || ran[continued] != 1 || len(ran) != 2 {
		t.Fatalf("mason turns %v", ran)
	}
	if th := f.Thread(t, stream, intake.MasonAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Request.TurnID != continued {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	if s, ok := streak(f, t, intake.MasonRole); ok {
		t.Fatalf("a behavioural failure made a streak: %+v", s)
	}
	fake.Check(t)
}

func TestReviewerInfrastructureFailureContestsUnit(t *testing.T) {
	t.Parallel()
	f, fake := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.Stop(t)
	fake.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Built")
	manager := &intakefixture.Manager{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	var mu sync.Mutex
	crash, reviews := true, map[string]int{}
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			return manager.Turn(ctx, req, verified, tools)
		}
		mu.Lock()
		reviews[req.Name]++
		crashing := crash
		mu.Unlock()
		if crashing {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Crashed", SessionDir: req.SessionDir, NumTurns: 1, ExitCode: 1}, nil
		}
		body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("verdict %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "crashing-reviewer", fixturekit.ValidPlan)
	f.AwaitUnit(t, stream, "resume", intake.UnitContested)
	th := f.Thread(t, stream, intake.ReviewerAgent("resume"))
	q := awaitCompleted(f, t, stream, intake.ReviewerAgent("resume"), th.Turns[len(th.Turns)-1].Request.TurnID)
	checkExhausted(t, q)
	checkFailureContest(t, f, stream, "resume", intake.UnitReviewing, q)
	mu.Lock()
	if reviews[q.Request.TurnID] != service.TurnRetries+1 {
		t.Fatalf("review attempts %v", reviews)
	}
	crash = false
	mu.Unlock()
	if s, ok := streak(f, t, intake.ReviewerRole); !ok || s.Consecutive != service.TurnRetries+1 {
		t.Fatalf("reviewer streak %+v %t", s, ok)
	}

	if _, err := f.C.RuleContested(context.Background(), stream, "resume", "revise", "Rework it"); err == nil || !strings.Contains(err.Error(), "use review") {
		t.Fatalf("revise on a failed review: %v", err)
	}
	ruling, err := f.C.RuleContested(context.Background(), stream, "resume", "review", "The reviewer is back")
	if err != nil || ruling.Ruling.Decision != "review" || ruling.Ruling.Contest != execution.FailureContestID(q.Response.ID) {
		t.Fatalf("review ruling %+v %v", ruling, err)
	}
	if _, err := f.C.RuleContested(context.Background(), stream, "resume", "review", "Again"); err == nil {
		t.Fatal("accepted a second ruling")
	}
	intakefixture.Soon(t, "the unit to be reviewed again", func() bool {
		_, err := approvedReviewResult(f, stream, "resume")
		return err == nil
	})
	th = f.Thread(t, stream, intake.ReviewerAgent("resume"))
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID == q.Request.TurnID || th.Turns[1].Status() != "idle" {
		t.Fatalf("reviewer turns %+v", th.Turns)
	}
	if s, ok := streak(f, t, intake.ReviewerRole); ok {
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

func streak(f *journey.ShedFixture, t *testing.T, role string) (api.FailureStreak, bool) {
	t.Helper()
	all, err := f.C.Statuses(context.Background())
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
	if q.Status() != "failed" || q.Response.FailureClass != coreadapter.Infrastructure || len(q.Attempts) != service.TurnRetries+1 {
		t.Fatalf("turn %s ended %s with %+v after %d attempts", q.Request.TurnID, q.Status(), q.Response, len(q.Attempts))
	}
	for _, a := range q.Attempts {
		if a.FailureClass != coreadapter.Infrastructure || a.Profile.Name != "default" || a.Failure == "" {
			t.Fatalf("attempt %+v", a)
		}
	}
}

func checkFailureContest(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, unit, from string, q trace.QueuedTurn) {
	t.Helper()
	status, err := f.C.Status(context.Background(), stream)
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
	for _, want := range []string{"failed on every retry and fallback profile", q.Request.TurnID, fmt.Sprintf("%d on profile default: infrastructure failure", service.TurnRetries+1)} {
		if !strings.Contains(reason, want) {
			t.Fatalf("contested status %+v lacks %q", status.Units, want)
		}
	}
	id := execution.FailureContestID(q.Response.ID)
	var contest trace.Transition
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.ID == id {
			contest = tr
		}
	}
	if contest.From != from || contest.To != intake.UnitContested || contest.Cause != q.Response.ID || contest.Reason != reason {
		t.Fatalf("contest transition %+v", contest)
	}
	outbox, err := f.Repository().Outbox(stream)
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

func approvedReviewResult(f *journey.ShedFixture, stream config.WorkstreamID, unit string) (service.UnitReviewResult, error) {
	r := &journey.Reviewers{Masons: &journey.Masons{Repository: f.Repository()}}
	result, ok, err := r.Driver().StoredResult(stream, unit, trace.WorkflowState{})
	if err != nil {
		return result, err
	}
	if !ok || result.Verdict.Decision != "satisfactory" {
		return result, fmt.Errorf("no satisfactory review")
	}
	return result, nil
}
