package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const climbLoopSpec = `{
  "version": 1,
  "goal": "Make the parser faster on the corpus.",
  "evaluator": "parse-bench",
  "metric": {"name": "wall_ms", "direction": "lower", "min_improvement": 5},
  "guardrails": {"limits": [{"metric": "peak_rss_mb", "direction": "lower", "max": 200}]},
  "protected": {"fixtures": ["testdata/corpus"]},
  "stop": {"max_attempts": 3}
}`

func (c *climbFixture) playAttempt(n int, files map[string]string, hypothesis string) {
	turn := fmt.Sprintf("climb-mason-%d-1", n)
	engine := c.shedFixture.engine
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	engine.Turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		c.recordPrompt(req)
		view := req.Workspace.Directory()
		for name, content := range files {
			path := filepath.Join(view, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return nil, err
			}
		}
		body, err := fixturekit.CallTool(ctx, tools, "done", map[string]any{"hypothesis": hypothesis, "change": "writes " + fmt.Sprint(len(files)) + " file(s)", "expected_effect": "less wall time"})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("done %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Done", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
}

func (c *climbFixture) diagnose(t *testing.T, stream config.WorkstreamID) {
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		repo := c.repository()
		if open, found, err := climb.OpenAttempt(repo, stream); err == nil && found {
			t.Logf("open attempt %d is %s: %+v", open.Number, open.Phase, open)
		}
		threads, err := repo.Threads(stream)
		if err != nil {
			t.Logf("threads: %v", err)
		}
		for _, th := range threads {
			for _, q := range th.Turns {
				out := ""
				failure := ""
				if q.Response != nil {
					failure = q.Response.Failure
					if q.Response.Result.Outcome != nil {
						out = q.Response.Result.Outcome.Status
					}
				}
				t.Logf("thread %s turn %s: %s outcome=%q failure=%q attempts=%d", th.Identity.ID, q.Request.TurnID, q.Status(), out, failure, len(q.Attempts))
			}
		}
		for _, a := range c.attemptsRecorded(t, stream) {
			t.Logf("attempt %d: %s %s %s", a.Number, a.Outcome, a.Cause, a.Reason)
		}
	})
}

func (c *climbFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(c.clone), append([]string{"-C", c.clone}, args...)...))
}

func (c *climbFixture) attemptsRecorded(t *testing.T, stream config.WorkstreamID) []climb.Attempt {
	t.Helper()
	got, err := climb.ReadAttempts(c.repository(), stream)
	fixturekit.Must(t, err)
	return got
}

func within(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *climbFixture) awaitAttempts(t *testing.T, stream config.WorkstreamID, n int) {
	t.Helper()
	within(t, fmt.Sprintf("%d attempts to be recorded", n), func() bool { return len(c.attemptsRecorded(t, stream)) >= n })
}

func (c *climbFixture) awaitClimb(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	within(t, fmt.Sprintf("the climb to be %s (it is %s)", want, c.inner(t, stream)), func() bool { return c.inner(t, stream) == want })
}

func TestAClimbRunsAttemptsOnOneMasonThreadAndLandsTheKeptOneAsASquashedCommit(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101, 90, 95)
	c := newClimbFixtureWith(t, engine, false, nil, climbLoopSpec)
	defer c.stop(t)
	c.playAttempt(1, map[string]string{"src/parse.go": "package src\n// faster\n"}, "cache the parsed corpus")
	c.playAttempt(2, map[string]string{"src/other.go": "package src\n", "testdata/corpus/a.txt": "doctored\n"}, "shrink the corpus")
	c.playAttempt(3, map[string]string{"src/third.go": "package src\n// third\n"}, "batch the reads")
	stream := c.ratified(t)
	c.diagnose(t, stream)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	card, open := c.card(t)
	if !open {
		t.Fatal("no approval card")
	}
	evaluator := card.Answer.Body["commit"].(string)
	if _, err := climb.Steer(context.Background(), c.repository(), stream, "prefer caching over parallelism", "chief", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.c.ApproveClimb(context.Background(), stream, api.ClimbApprovalRequest{Decision: "approve", Commit: evaluator, SpecRevision: 1}); err != nil {
		t.Fatal(err)
	}
	c.awaitClimb(t, stream, climb.InnerConcluding)

	attempts := c.attemptsRecorded(t, stream)
	if len(attempts) != 3 {
		t.Fatalf("recorded attempts: %+v", attempts)
	}
	first := attempts[0]
	if first.Outcome != climb.OutcomeKept || first.Parent != evaluator || first.Best != 101 || first.Scores == nil || first.Scores.Primary != 90 || first.Hypothesis != "cache the parsed corpus" {
		t.Fatalf("attempt 1: %+v", first)
	}
	branch := intake.FeatureBranch(stream)
	if tip := c.git(t, "rev-parse", branch); tip != first.Commit {
		t.Fatalf("the climb branch is at %s, not at the kept commit %s", tip, first.Commit)
	}
	if parent := c.git(t, "rev-parse", first.Commit+"^"); parent != evaluator {
		t.Fatalf("the kept commit's parent is %s, not the evaluator commit %s", parent, evaluator)
	}
	if got := c.git(t, "show", "--name-only", "--format=", first.Commit); got != "src/parse.go" {
		t.Fatalf("the kept commit changes %q", got)
	}
	second := attempts[1]
	if second.Outcome != climb.OutcomeDiscarded || second.Cause != climb.CauseProtected || second.Measurement != nil || second.Parent != first.Commit || second.Best != 90 {
		t.Fatalf("attempt 2: %+v", second)
	}
	third := attempts[2]
	if third.Outcome != climb.OutcomeDiscarded || third.Cause != climb.CauseNoImprovement || third.Parent != first.Commit || third.Scores == nil || third.Scores.Primary != 95 {
		t.Fatalf("attempt 3: %+v", third)
	}
	if got := engine.count(); got != 5 {
		t.Fatalf("the evaluator ran %d times: 3 calibration runs and one measurement of each of two attempts", got)
	}
	for _, a := range attempts[1:] {
		want := execution.UnitBranch(stream, climb.AttemptUnit(a.Number))
		if a.Branch != want || c.git(t, "rev-parse", "--verify", want) != a.Candidate {
			t.Errorf("attempt %d: branch %q holds %q, candidate %s", a.Number, a.Branch, c.git(t, "rev-parse", "--verify", want), a.Candidate)
		}
	}
	if worktrees := c.git(t, "worktree", "list"); strings.Contains(worktrees, "climb-attempt") {
		t.Fatalf("workspaces left behind:\n%s", worktrees)
	}
	th, err := c.repository().Thread(stream, execution.ClimbMasonAgent)
	fixturekit.Must(t, err)
	var turns []string
	for _, q := range th.Turns {
		turns = append(turns, q.Request.TurnID)
	}
	if !slices.Equal(turns, []string{"climb-mason-1-1", "climb-mason-2-1", "climb-mason-3-1"}) {
		t.Fatalf("the climb's mason thread ran %v", turns)
	}
	p1, _ := c.promptOf("climb-mason-1-1")
	p3, _ := c.promptOf("climb-mason-3-1")
	for _, want := range []string{"prefer caching over parallelism (new)", "No attempt has been recorded yet"} {
		if !strings.Contains(p1, want) {
			t.Errorf("the first attempt's bundle lacks %q", want)
		}
	}
	for _, want := range []string{"- attempt 1: kept", "- attempt 2: discarded (protected-path)", "cache the parsed corpus", "shrink the corpus", first.Commit, "prefer caching over parallelism"} {
		if !strings.Contains(p3, want) {
			t.Errorf("the third attempt's bundle lacks %q:\n%s", want, p3)
		}
	}
	docs, err := trace.Read[trace.Document](c.repository(), stream)
	fixturekit.Must(t, err)
	records := map[string]int{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, trace.ClimbAttemptsDir+"/") {
			records[d.Path]++
		}
	}
	if len(records) != 3 || records[trace.ClimbAttemptPath(1)] != 1 {
		t.Fatalf("terminal records %v", records)
	}
	if kept := c.chiefNotices(t, stream, trace.ProgressKind); !slices.ContainsFunc(kept, func(b string) bool { return strings.Contains(b, "Attempt 1 was kept") }) {
		t.Fatalf("the chief was not told of the kept attempt: %v", kept)
	}
	stops := c.chiefNotices(t, stream, trace.NoticeKind)
	if !slices.ContainsFunc(stops, func(b string) bool {
		return strings.Contains(b, "The climb concludes after attempt 3") && strings.Contains(b, "max_attempts")
	}) {
		t.Fatalf("the chief was not told of the stop: %v", stops)
	}
	running, found, err := climb.ReadRunning(c.repository(), stream)
	fixturekit.Must(t, err)
	if !found || running.Phase != "" {
		t.Fatalf("running %+v", running)
	}
}

func TestARestartDuringAClimbKeepsOneRecordPerAttemptAndTheSameThread(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101, 90, 80, 70)
	c := newClimbFixtureWith(t, engine, false, nil, climbLoopSpec)
	defer c.stop(t)
	for n := 1; n <= 3; n++ {
		c.playAttempt(n, map[string]string{fmt.Sprintf("src/a%d.go", n): "package src\n"}, fmt.Sprintf("idea %d", n))
	}
	stream := c.ratified(t)
	c.diagnose(t, stream)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	card, _ := c.card(t)
	if _, err := c.c.ApproveClimb(context.Background(), stream, approvalOf(card)); err != nil {
		t.Fatal(err)
	}
	c.awaitAttempts(t, stream, 1)
	c.restart(t)
	c.awaitClimb(t, stream, climb.InnerConcluding)

	attempts := c.attemptsRecorded(t, stream)
	if len(attempts) != 3 {
		t.Fatalf("recorded attempts %+v", attempts)
	}
	var lastKept string
	for i, a := range attempts {
		if a.Number != i+1 {
			t.Fatalf("attempt numbers: %+v", attempts)
		}
		if a.Outcome == climb.OutcomeKept {
			lastKept = a.Commit
		}
		if i > 0 && a.Parent != attempts[i-1].Commit && attempts[i-1].Outcome == climb.OutcomeKept {
			t.Errorf("attempt %d started from %s, not the best %s", a.Number, a.Parent, attempts[i-1].Commit)
		}
	}
	if tip := c.git(t, "rev-parse", intake.FeatureBranch(stream)); tip != lastKept {
		t.Fatalf("the climb branch is at %s, the last kept commit is %s", tip, lastKept)
	}
	th, err := c.repository().Thread(stream, execution.ClimbMasonAgent)
	fixturekit.Must(t, err)
	ids := map[string]bool{}
	for _, q := range th.Turns {
		ids[q.Request.TurnID] = true
	}
	for n := 1; n <= 3; n++ {
		if !ids[fmt.Sprintf("climb-mason-%d-1", n)] {
			t.Errorf("attempt %d has no turn on the climb's thread: %v", n, ids)
		}
	}
	if got := engine.count(); got < 6 || got > 7 {
		t.Fatalf("the evaluator ran %d times", got)
	}
}

func TestTheOwnerConcludesAndExtendsAClimbAndTheChiefSteersAndConcludesIt(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101)
	c := newClimbFixtureWith(t, engine, false, nil, climbLoopSpec)
	defer c.stop(t)
	stream := c.ratified(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	ctx := context.Background()
	if _, err := c.c.ExtendClimb(ctx, stream, api.ClimbExtendRequest{Attempts: 2}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("extending a climb that is not climbing: %v", err)
	}
	if _, err := c.c.ConcludeClimb(ctx, stream, api.ClimbConcludeRequest{}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("concluding a climb that is not climbing: %v", err)
	}
	card, _ := c.card(t)
	c.engineHold(t)
	if _, err := c.c.ApproveClimb(ctx, stream, approvalOf(card)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.c.ExtendClimb(ctx, stream, api.ClimbExtendRequest{}); !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("an empty extension: %v", err)
	}
	out, err := c.c.ExtendClimb(ctx, stream, api.ClimbExtendRequest{Attempts: 2, CostUSD: 1.5, WallClock: "1h", Note: "worth a few more"})
	fixturekit.Must(t, err)
	if out.Extension != 1 || out.Attempts != 2 {
		t.Fatalf("extension %+v", out)
	}
	budget, err := climb.ReadBudget(c.repository(), stream)
	fixturekit.Must(t, err)
	if total := budget.Total(); len(budget.Extensions) != 1 || total.Attempts != 2 || total.CostUSD != 1.5 {
		t.Fatalf("budget %+v", budget)
	}
	read, apiErr := c.s.climbProgress(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if got := read.Progress.Budgets; got.Attempts.Max != 5 || got.CostUSD.Max != 0 || got.WallClockSeconds.Max != 0 {
		t.Fatalf("the progress read shows %+v after the owner added two attempts to a limit of three", got)
	}
	if notices := c.chiefNotices(t, stream, trace.ProgressKind); !slices.ContainsFunc(notices, func(b string) bool { return strings.Contains(b, "extended the climb's budget") }) {
		t.Fatalf("the chief was not told of the extension: %v", notices)
	}
	if _, err := c.c.ConcludeClimb(ctx, stream, api.ClimbConcludeRequest{Note: "enough"}); err != nil {
		t.Fatal(err)
	}
	if got := c.inner(t, stream); got != climb.InnerConcluding {
		t.Fatalf("inner state %q", got)
	}
	if _, err := c.c.ExtendClimb(ctx, stream, api.ClimbExtendRequest{Attempts: 1}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("extending a concluding climb: %v", err)
	}
}

func (c *climbFixture) engineHold(t *testing.T) {
	t.Helper()
	engine := c.shedFixture.engine
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	for n := 1; n <= 3; n++ {
		engine.Turns[fmt.Sprintf("climb-mason-%d-1", n)] = intakefixture.HoldMasonTurn
	}
}

func climbStopFixture(t *testing.T, script func(jevfixture.Ask) jevfixture.Reply) (*Service, *trace.Repository, *jevfixture.Transitions, trace.WorkflowState) {
	t.Helper()
	p := &jevfixture.Transitions{Script: script}
	opts := transitionFixture(t)
	withTopLevel(t, opts, gateJevConfig(gateKey(t, true)))
	opts.JevProvider = p.Factory()
	s, _ := start(t, opts)
	repo := s.sole().repository
	ctx := context.Background()
	h := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
	}
	_, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("fixture-building"), Subject: trace.FeatureSubject, To: "building", Reason: "fixture"}})
	fixturekit.Must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("fixture-climbing"), Subject: climb.InnerSubject, To: climb.InnerClimbing, Reason: "fixture"}})
	fixturekit.Must(t, err)
	approval, err := json.MarshalIndent(climb.ApprovalRecord{Evaluator: "parse-bench", Commit: "evaluator", Baseline: 100, Units: map[string]string{"wall_ms": "ms"}, Noise: 1, Protected: []string{"dagger.toml"}, ApprovedBy: "owner", At: fixturekit.DemoStart}, "", "  ")
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.ClimbApprovalID, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}, Path: trace.ClimbApprovalPath, Content: string(approval) + "\n"}}))
	for n := 1; n <= 2; n++ {
		fixturekit.Must(t, climb.RecordAttempt(ctx, repo, fixturekit.Stream, climb.Attempt{Number: n, Parent: "parent", Best: 100, Hypothesis: "idea", Outcome: climb.OutcomeDiscarded, Cause: climb.CauseNoImprovement, Reason: "short", StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart}))
	}
	state, err := climb.ReadInner(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	return s, repo, p, state
}

func TestAPlateauStopMayBeContinuedByJevAtItsBarButABudgetStopMayNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plateau := []climb.Firing{{Rule: climb.RulePlateau, Reason: "2 attempts passed since the last improvement"}}
	budget := []climb.Firing{{Rule: climb.RuleAttempts, Hard: true, Reason: "2 attempts were recorded, reaching the limit of 2"}}
	toContinue := func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, "continue", 0.99, 0.99) }

	t.Run("a plateau Jev continues", func(t *testing.T) {
		s, repo, p, state := climbStopFixture(t, toContinue)
		out, err := s.raiseClimbStop(ctx, repo, climb.StopRequest{Stream: fixturekit.Stream, After: 2, Firings: plateau, State: state})
		fixturekit.Must(t, err)
		if out != climb.StopContinue || len(p.TransitionRequests()) != 1 {
			t.Fatalf("outcome %q after %d requests to Jev", out, len(p.TransitionRequests()))
		}
		if inner, _ := climb.ReadInner(repo, fixturekit.Stream); inner.Value != climb.InnerClimbing {
			t.Fatalf("inner state %q", inner.Value)
		}
		stops, err := climb.ReadStops(repo, fixturekit.Stream)
		fixturekit.Must(t, err)
		if rec := stops[2]; rec.Decision != "continue" || !slices.Equal(rec.Rules, []string{climb.RulePlateau}) {
			t.Fatalf("stop records %+v", stops)
		}
		again, err := s.raiseClimbStop(ctx, repo, climb.StopRequest{Stream: fixturekit.Stream, After: 2, Firings: plateau, State: state})
		fixturekit.Must(t, err)
		if again != climb.StopContinue || len(p.TransitionRequests()) != 1 {
			t.Fatalf("raising the same stop again: %q after %d requests", again, len(p.TransitionRequests()))
		}
	})
	t.Run("a plateau Jev lets end", func(t *testing.T) {
		s, repo, _, state := climbStopFixture(t, jevfixture.DefaultFits)
		out, err := s.raiseClimbStop(ctx, repo, climb.StopRequest{Stream: fixturekit.Stream, After: 2, Firings: plateau, State: state})
		fixturekit.Must(t, err)
		if inner, _ := climb.ReadInner(repo, fixturekit.Stream); out != climb.StopConclude || inner.Value != climb.InnerConcluding {
			t.Fatalf("outcome %q, inner %q", out, inner.Value)
		}
	})
	t.Run("a spent budget cannot be continued past", func(t *testing.T) {
		s, repo, p, state := climbStopFixture(t, toContinue)
		out, err := s.raiseClimbStop(ctx, repo, climb.StopRequest{Stream: fixturekit.Stream, After: 2, Firings: budget, State: state})
		fixturekit.Must(t, err)
		if inner, _ := climb.ReadInner(repo, fixturekit.Stream); out != climb.StopConclude || inner.Value != climb.InnerConcluding || len(p.TransitionRequests()) != 0 {
			t.Fatalf("outcome %q, inner %q, %d requests to Jev", out, inner.Value, len(p.TransitionRequests()))
		}
		notices := outboxBodies(t, repo, trace.NoticeKind)
		if !slices.ContainsFunc(notices, func(b string) bool { return strings.Contains(b, "The climb concludes after attempt 2") }) {
			t.Fatalf("notices %v", notices)
		}
	})
}

func outboxBodies(t *testing.T, repo *trace.Repository, kind string) []string {
	t.Helper()
	out, err := repo.Outbox(fixturekit.Stream)
	fixturekit.Must(t, err)
	var bodies []string
	for _, e := range out {
		if e.Event.Kind == kind {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

func TestARecordedClimbAttemptCountsAsLoopGuardProgress(t *testing.T) {
	t.Parallel()
	_, repo, _, _ := climbStopFixture(t, nil)
	later := fixturekit.DemoStart.Add(3 * time.Hour)
	fixturekit.Must(t, climb.RecordAttempt(context.Background(), repo, fixturekit.Stream, climb.Attempt{Number: 3, Parent: "parent", Best: 100, Hypothesis: "idea", Outcome: climb.OutcomeDiscarded, Cause: climb.CauseNoCandidate, Reason: "none", StartedAt: later, FinishedAt: later}))
	since, what, err := lastProgress(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !since.Equal(later) || what != "a recorded climb attempt" {
		t.Fatalf("last progress %s: %s", since, what)
	}
}

func TestTheChiefConcludesAClimbAndItsProgressIsInTheChiefsStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, repo, _, state := climbStopFixture(t, nil)
	spec, err := climb.Parse([]byte(climbLoopSpec), climb.Defaults{})
	fixturekit.Must(t, err)
	text, err := climbProgressText(repo, fixturekit.Stream, spec, fixturekit.DemoStart.Add(time.Hour))
	fixturekit.Must(t, err)
	for _, want := range []string{"The climb is climbing.", "2 attempts recorded, 0 kept.", "Attempts: 2 of 3 allowed."} {
		if !strings.Contains(text, want) {
			t.Errorf("the climb's progress lacks %q:\n%s", want, text)
		}
	}
	chief := trace.Actor{Kind: "agent", ID: trace.Manager}
	res, err := s.RouteExit(ctx, s.concludeExit(repo, fixturekit.Stream, state, chief, "the log shows no more ideas"))
	fixturekit.Must(t, err)
	if inner, _ := climb.ReadInner(repo, fixturekit.Stream); !res.Committed || inner.Value != climb.InnerConcluding {
		t.Fatalf("result %+v, inner %q", res, inner.Value)
	}
	notices := outboxBodies(t, repo, trace.NoticeKind)
	if !slices.ContainsFunc(notices, func(b string) bool {
		return strings.Contains(b, "concluded now by the chief of staff") && strings.Contains(b, "the log shows no more ideas")
	}) {
		t.Fatalf("notices %v", notices)
	}
}

func TestNoUpstreamDriftIsAskedForWhileAClimbIsBuilding(t *testing.T) {
	t.Parallel()
	c := newClimbFixtureWith(t, newEvaluatorEngine(100, 100, 100), false, nil, climbLoopSpec)
	defer c.stop(t)
	stream := c.ratified(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	ctx := context.Background()

	requested, err := DriftForTest(c.s, c.repository()).Request(ctx, func(config.WorkstreamID) (string, error) { return "the schedule is due", nil })
	if err != nil || len(requested) != 0 {
		t.Fatalf("the schedule asked for a drift rebase of a climb: %v %v", requested, err)
	}
	if _, err := c.c.RebaseWorkstream(ctx, stream); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "takes no upstream drift while building") {
		t.Fatalf("the owner's request for a drift rebase of a climb: %v", err)
	}
	out, err := c.c.RebaseProject(ctx, c.project)
	fixturekit.Must(t, err)
	if len(out.Covered) != 0 || len(out.Skipped) != 1 || out.Skipped[0].Workstream != stream || !strings.Contains(out.Skipped[0].Reason, "takes no upstream drift while building") {
		t.Fatalf("the project's drift rebase covers %+v and skips %+v", out.Covered, out.Skipped)
	}
	ops, err := c.repository().Operations(stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == DriftAction {
			t.Fatalf("a drift operation was recorded for a climb: %+v", o)
		}
	}
}
