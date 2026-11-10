package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"
	utf8 "unicode/utf8"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	status "github.com/kpenfound/osmia/internal/status"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// continueTurn is the ID of the turn that continues the mason turn of the
// given sequence.
func continueTurn(unit string, sequence int) string {
	return fmt.Sprintf("%s-continue-%d", masonAgent(unit), sequence)
}

// playContinuation has the mason's turn of the given name play play, and
// registers the turn with the fixture's engine.
func playContinuation(f *shedFixture, fake *fakeMasons, name string, play func(context.Context, agent.Request, *mcp.ClientSession) error) {
	fake.Play[name] = play
	f.engine.Mu.Lock()
	f.engine.Turns[name] = fake.Turn
	f.engine.Mu.Unlock()
}

// checkingMoves counts the transitions that moved the unit to checking.
func checkingMoves(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) int {
	t.Helper()
	n := 0
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.Subject == trace.UnitSubject(unit) && tr.To == UnitChecking {
			n++
		}
	}
	return n
}

// onlyReport returns the one recorded report of the unit.
func onlyReport(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) UnitReport {
	t.Helper()
	docs := f.reports(t, stream, unit)
	if len(docs) != 1 {
		t.Fatalf("reports of unit %s: %+v", unit, docs)
	}
	var report UnitReport
	must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	return report
}

func TestMasonDoneWithALongCardIsRecorded(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	long := strings.Repeat("The storage client resumes uploads from the last chunk. ", 6)
	fake.Play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built", "happened": long})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	stream := f.seedBuilding(t, "long-card", validPlan)
	f.awaitUnit(t, stream, "resume", UnitReviewing)
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
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", validPlan, "", func(opts *Options) {
		// Stop the pass that queues the continuation once it is durable,
		// as a service stopping before it finished the decision does.
		opts.boundary = func(_ context.Context, step string) error {
			if strings.HasPrefix(step, "mason-turn-queued:") && strings.HasSuffix(step, ":resume") && crash.Add(1) == 2 {
				return errors.New("the service stopped")
			}
			return nil
		}
	})
	defer f.stop(t)
	var refused atomic.Pointer[string]
	fake.Play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := done(ctx, tools, map[string]any{"outcome": " "})
		if err != nil || recorded {
			return fmt.Errorf("done was not refused: recorded %t, %v", recorded, err)
		}
		refused.Store(&reason)
		return nil
	}
	playContinuation(f, fake, continueTurn("resume", 1), reportDone("Built"))
	stream := f.seedBuilding(t, "refused-done", validPlan)
	f.awaitUnit(t, stream, "resume", UnitReviewing)

	first := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
	refusal := *refused.Load()
	if first.Response.Result.Refusal == "" || first.Response.Result.Refusal != refusal || first.Response.Classification != nil {
		t.Fatalf("the refused turn recorded refusal %q and classification %+v, want refusal %q", first.Response.Result.Refusal, first.Response.Classification, refusal)
	}
	th := f.thread(t, stream, masonAgent("resume"))
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

	// A restart changes nothing: the decision was recorded once.
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	if th := f.thread(t, stream, masonAgent("resume")); len(th.Turns) != 2 {
		t.Fatalf("mason turns after a restart %+v", th.Turns)
	}
	if n := checkingMoves(t, f, stream, "resume"); n != 1 {
		t.Fatalf("moves to checking after a restart: %d", n)
	}
	fake.Check(t)
}

func TestMasonFailureWithoutDoneGetsAContinuation(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	fake.Play[masonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error { return errFailTurn }
	playContinuation(f, fake, continueTurn("resume", 1), reportDone("Built"))
	stream := f.seedBuilding(t, "failed-turn", validPlan)
	f.awaitUnit(t, stream, "resume", UnitReviewing)

	th := f.thread(t, stream, masonAgent("resume"))
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
			f, fake := newMasonFixture(t, 1, validPlan)
			defer f.stop(t)
			name := masonTurnID("resume")
			fake.Play[name] = reportDone("Built")
			if tc.end == nil {
				fake.Play[name] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
					if err := reportDone("Built")(ctx, req, tools); err != nil {
						return err
					}
					return errFailTurn
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
			stream := f.seedBuilding(t, "done-then-failure", validPlan)
			f.awaitUnit(t, stream, "resume", UnitReviewing)

			th := f.thread(t, stream, masonAgent("resume"))
			if len(th.Turns) != 1 || th.Turns[0].Status() != "failed" {
				t.Fatalf("the mason's turns %+v", th.Turns)
			}
			if outcome := th.Turns[0].Response.Result.Outcome; outcome == nil || outcome.Status != masonDone {
				t.Fatalf("the failed turn's outcome %+v", outcome)
			}
			if report := onlyReport(t, f, stream, "resume"); report.Outcome != "Built" || report.Turn != name {
				t.Fatalf("report %+v", report)
			}
			if n := checkingMoves(t, f, stream, "resume"); n != 1 {
				t.Fatalf("moves to checking: %d", n)
			}
			for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
				if tr.To == UnitContested {
					t.Fatalf("the unit was contested: %+v", tr)
				}
			}

			f.stop(t)
			f.start(t)
			f.awaitUnit(t, stream, "resume", UnitReviewing)
			if th := f.thread(t, stream, masonAgent("resume")); len(th.Turns) != 1 {
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
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	bound := f.s.cfg.Mason.MaxCleanTurns
	// The first turn is refused, the continuations fail: every ending counts.
	fake.Play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, _, err := done(ctx, tools, map[string]any{"outcome": " "}); err != nil || recorded {
			return fmt.Errorf("done was not refused: %t %v", recorded, err)
		}
		return nil
	}
	for i := 1; i < bound; i++ {
		playContinuation(f, fake, continueTurn("resume", i), func(context.Context, agent.Request, *mcp.ClientSession) error { return errFailTurn })
	}
	stream := f.seedBuilding(t, "bounded", validPlan)
	f.awaitUnit(t, stream, "resume", UnitContested)

	th := f.thread(t, stream, masonAgent("resume"))
	if len(th.Turns) != bound {
		t.Fatalf("the mason had %d turns, want %d", len(th.Turns), bound)
	}
	last := f.awaitCompleted(t, stream, masonAgent("resume"), th.Turns[bound-1].Request.TurnID)
	id := failureContestID(last.Response.ID)
	var contest trace.Transition
	contests := 0
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.To == UnitContested {
			contest = tr
			contests++
		}
	}
	reason := "mason.max_clean_turns"
	if contests != 1 || contest.ID != id || contest.From != UnitImplementing || contest.Cause != last.Response.ID || !strings.Contains(contest.Reason, reason) || !strings.Contains(contest.Reason, "it failed") {
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	contests = 0
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.To == UnitContested {
			contests++
		}
	}
	if th := f.thread(t, stream, masonAgent("resume")); contests != 1 || len(th.Turns) != bound {
		t.Fatalf("after a restart: %d contests, %d turns", contests, len(th.Turns))
	}
	fake.Check(t)
}
