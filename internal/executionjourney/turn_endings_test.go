package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	utf8 "unicode/utf8"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	status "github.com/kpenfound/osmia/internal/status"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func continueTurn(unit string, sequence int) string {
	return fmt.Sprintf("%s-continue-%d", intake.MasonAgent(unit), sequence)
}

func playContinuation(f *shedFixture, fake *intakefixture.FakeMasons, name string, play func(context.Context, agent.Request, *mcp.ClientSession) error) {
	fake.Play[name] = play
	f.engine.Mu.Lock()
	f.engine.Turns[name] = fake.Turn
	f.engine.Mu.Unlock()
}

func checkingMoves(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) int {
	t.Helper()
	n := 0
	for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.Subject == trace.UnitSubject(unit) && tr.To == execution.UnitChecking {
			n++
		}
	}
	return n
}

func onlyReport(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) execution.UnitReport {
	t.Helper()
	docs := f.reports(t, stream, unit)
	if len(docs) != 1 {
		t.Fatalf("reports of unit %s: %+v", unit, docs)
	}
	var report execution.UnitReport
	fixturekit.Must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	return report
}

func TestMasonDoneWithALongCardIsRecorded(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.stop(t)
	long := strings.Repeat("The storage client resumes uploads from the last chunk. ", 6)
	fake.Play[service.TestMasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Built", "happened": long})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	stream := f.seedBuilding(t, "long-card", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
	report := onlyReport(t, f, stream, "resume")
	if report.Outcome != "Built" || report.Card == nil {
		t.Fatalf("report %+v", report)
	}
	if n := utf8.RuneCountInString(report.Card.Happened); n > 140 || !strings.HasSuffix(report.Card.Happened, "…") {
		t.Fatalf("stored happened (%d characters) %q", n, report.Card.Happened)
	}
	if _, err := status.CheckCard(*report.Card, nil); err != nil {
		t.Fatalf("stored card %+v: %v", report.Card, err)
	}
	if n := checkingMoves(t, f, stream, "resume"); n != 1 {
		t.Fatalf("moves to checking: %d", n)
	}
	fake.Check(t)
}

func TestMasonRefusedDoneGetsOneContinuationWithTheReason(t *testing.T) {
	t.Parallel()
	var crash atomic.Int64
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", fixturekit.ValidPlan, func(opts *Options) {

		opts.boundary = func(_ context.Context, step string) error {
			if strings.HasPrefix(step, "mason-turn-queued:") && strings.HasSuffix(step, ":resume") && crash.Add(1) == 2 {
				return errors.New("the service stopped")
			}
			return nil
		}
	})
	defer f.stop(t)
	var refused atomic.Pointer[string]
	fake.Play[service.TestMasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": " "})
		if err != nil || recorded {
			return fmt.Errorf("done was not refused: recorded %t, %v", recorded, err)
		}
		refused.Store(&reason)
		return nil
	}
	playContinuation(f, fake, continueTurn("resume", 1), intakefixture.ReportDone("Built"))
	stream := f.seedBuilding(t, "refused-done", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)

	first := f.awaitCompleted(t, stream, intake.MasonAgent("resume"), service.TestMasonTurnID("resume"))
	refusal := *refused.Load()
	if first.Response.Result.Refusal == "" || first.Response.Result.Refusal != refusal || first.Response.Classification != nil {
		t.Fatalf("the refused turn recorded refusal %q and classification %+v, want refusal %q", first.Response.Result.Refusal, first.Response.Classification, refusal)
	}
	th := f.thread(t, stream, intake.MasonAgent("resume"))
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != continueTurn("resume", 1) || th.Turns[1].Request.ThreadID != th.Identity.ThreadID {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	prompt := th.Turns[1].Request.Prompt
	if !strings.Contains(prompt, refusal) || !strings.Contains(prompt, "call done again") || strings.Contains(prompt, "without calling an outcome tool") {
		t.Fatalf("continuation prompt %q does not state the refusal %q", prompt, refusal)
	}
	if n := checkingMoves(t, f, stream, "resume"); n != 1 {
		t.Fatalf("moves to checking: %d", n)
	}

	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
	if th := f.thread(t, stream, intake.MasonAgent("resume")); len(th.Turns) != 2 {
		t.Fatalf("mason turns after a restart %+v", th.Turns)
	}
	if n := checkingMoves(t, f, stream, "resume"); n != 1 {
		t.Fatalf("moves to checking after a restart: %d", n)
	}
	fake.Check(t)
}

func TestMasonFailureWithoutDoneGetsAContinuation(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.stop(t)
	fake.Play[service.TestMasonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrFailTurn }
	playContinuation(f, fake, continueTurn("resume", 1), intakefixture.ReportDone("Built"))
	stream := f.seedBuilding(t, "failed-turn", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)

	th := f.thread(t, stream, intake.MasonAgent("resume"))
	if len(th.Turns) != 2 {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	if first := th.Turns[0]; first.Status() != "failed" || first.Response.FailureClass != coreadapter.Behavioural {
		t.Fatalf("the first turn ended %s with %+v", first.Status(), first.Response)
	}
	if q := th.Turns[1]; q.Request.TurnID != continueTurn("resume", 1) || !strings.Contains(q.Request.Prompt, "ended without a report") || !strings.Contains(q.Request.Prompt, "it failed:") {
		t.Fatalf("continuation %+v", q.Request)
	}
	if n := checkingMoves(t, f, stream, "resume"); n != 1 {
		t.Fatalf("moves to checking: %d", n)
	}
	fake.Check(t)
}

func TestMasonAcceptedDoneBeatsTheFailureThatEndedTheTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(*agent.Result)
	}{
		{"behavioural failure", nil},
		{"error ending", func(r *agent.Result) { r.IsError, r.ErrorSubtype = true, "execution" }},
		{"turn limit", func(r *agent.Result) { r.IsError, r.ErrorSubtype = true, "error_max_turns" }},
		{"time limit", func(r *agent.Result) { r.TimedOut = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
			defer f.stop(t)
			name := service.TestMasonTurnID("resume")
			fake.Play[name] = intakefixture.ReportDone("Built")
			if tc.end == nil {
				fake.Play[name] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
					if err := intakefixture.ReportDone("Built")(ctx, req, tools); err != nil {
						return err
					}
					return intakefixture.ErrFailTurn
				}
			}
			f.engine.Mu.Lock()
			f.engine.Turns[name] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				result, err := fake.Turn(ctx, req, verified, tools)
				if result != nil && tc.end != nil {
					tc.end(result)
				}
				return result, err
			}
			f.engine.Mu.Unlock()
			stream := f.seedBuilding(t, "done-then-failure", fixturekit.ValidPlan)
			f.awaitUnit(t, stream, "resume", intake.UnitReviewing)

			th := f.thread(t, stream, intake.MasonAgent("resume"))
			if len(th.Turns) != 1 || th.Turns[0].Status() != "failed" {
				t.Fatalf("the mason's turns %+v", th.Turns)
			}
			if outcome := th.Turns[0].Response.Result.Outcome; outcome == nil || outcome.Status != execution.MasonDone {
				t.Fatalf("the failed turn's outcome %+v", outcome)
			}
			if report := onlyReport(t, f, stream, "resume"); report.Outcome != "Built" || report.Turn != name {
				t.Fatalf("report %+v", report)
			}
			if n := checkingMoves(t, f, stream, "resume"); n != 1 {
				t.Fatalf("moves to checking: %d", n)
			}
			for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
				if tr.To == intake.UnitContested {
					t.Fatalf("the unit was contested: %+v", tr)
				}
			}

			f.stop(t)
			f.start(t)
			f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
			if th := f.thread(t, stream, intake.MasonAgent("resume")); len(th.Turns) != 1 {
				t.Fatalf("mason turns after a restart %+v", th.Turns)
			}
			if n := checkingMoves(t, f, stream, "resume"); n != 1 {
				t.Fatalf("moves to checking after a restart: %d", n)
			}
			fake.Check(t)
		})
	}
}

func TestMasonEndingsWithoutAReportContestTheUnitOnceTheBoundIsUsedUp(t *testing.T) {
	t.Parallel()
	passed, prepare := countingSchedule()
	f, fake := newMasonFixturePrepared(t, 1, fixturekit.ValidPlan, prepare)
	defer f.stop(t)
	bound := f.s.cfg.Mason.MaxCleanTurns

	fake.Play[service.TestMasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, _, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": " "}); err != nil || recorded {
			return fmt.Errorf("done was not refused: %t %v", recorded, err)
		}
		return nil
	}
	for i := 1; i < bound; i++ {
		playContinuation(f, fake, continueTurn("resume", i), func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrFailTurn })
	}
	stream := f.seedBuilding(t, "bounded", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitContested)

	th := f.thread(t, stream, intake.MasonAgent("resume"))
	if len(th.Turns) != bound {
		t.Fatalf("the mason had %d turns, want %d", len(th.Turns), bound)
	}
	last := f.awaitCompleted(t, stream, intake.MasonAgent("resume"), th.Turns[bound-1].Request.TurnID)
	id := execution.FailureContestID(last.Response.ID)
	var contest trace.Transition
	contests := 0
	for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.To == intake.UnitContested {
			contest = tr
			contests++
		}
	}
	reason := "mason.max_clean_turns"
	if contests != 1 || contest.ID != id || contest.From != intake.UnitImplementing || contest.Cause != last.Response.ID || !strings.Contains(contest.Reason, reason) || !strings.Contains(contest.Reason, "it failed") {
		t.Fatalf("contests %d: %+v", contests, contest)
	}
	if body := f.notice(t, stream, id); !strings.Contains(body, "is contested") || !strings.Contains(body, reason) {
		t.Fatalf("contest notice %q", body)
	}
	if checkingMoves(t, f, stream, "resume") != 0 {
		t.Fatal("the unit moved to checking")
	}

	f.stop(t)
	f.start(t)
	intakefixture.AwaitFurtherPasses(t, passed, 3)
	contests = 0
	for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.To == intake.UnitContested {
			contests++
		}
	}
	if th := f.thread(t, stream, intake.MasonAgent("resume")); contests != 1 || len(th.Turns) != bound {
		t.Fatalf("after a restart: %d contests, %d turns", contests, len(th.Turns))
	}
	fake.Check(t)
}
