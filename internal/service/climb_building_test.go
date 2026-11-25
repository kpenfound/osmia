package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	climbBuildingSkill = "https://github.com/kpenfound/skills#hill-climb-evaluator"
	climbBuildingSpec  = `{
  "version": 1,
  "goal": "Make the parser faster on the corpus.",
  "evaluator": "parse-bench",
  "metric": {"name": "wall_ms", "direction": "lower", "min_improvement": 5},
  "guardrails": {"limits": [{"metric": "peak_rss_mb", "direction": "lower", "max": 200}]},
  "protected": {"fixtures": ["testdata/corpus"]},
  "stop": {"max_attempts": 20}
}`
	climbDevModuleToml = "[modules.bench]\nsource = \".dagger/bench\"\nmain = true\n"
	climbNoEvaluator   = "package bench\n\n// no evaluator yet\n"
	climbEvaluator     = "package bench\n\nfunc (m *Bench) ParseBench(run string) string { return measure(run, \"wall_ms\", \"peak_rss_mb\") }\n"
)

func benchOutput(wall float64) string {
	return fmt.Sprintf(`{"schema":1,"valid":true,"reason":"ok","metrics":{"wall_ms":{"value":%v,"unit":"ms","direction":"lower"},"peak_rss_mb":{"value":120,"unit":"MB","direction":"lower"}}}`, wall)
}

type evaluatorCall struct {
	dir, function, run string
	files              map[string]string
}

// evaluatorEngine is the check client of a fixture: it passes every check and
// calls the evaluator function by replying with the scripted output of the
// call, recording what the export of the call held.
type evaluatorEngine struct {
	ReviewChecks
	mu    sync.Mutex
	calls []evaluatorCall
	reply func(n int) checks.CallResult
}

func newEvaluatorEngine(walls ...float64) *evaluatorEngine {
	e := &evaluatorEngine{ReviewChecks: fixturekit.PassingChecks}
	e.reply = func(n int) checks.CallResult {
		wall := 100.0
		if n <= len(walls) {
			wall = walls[n-1]
		}
		return checks.CallResult{Stdout: benchOutput(wall)}
	}
	return e
}

func (e *evaluatorEngine) Call(_ context.Context, dir, function, run string) (checks.CallResult, error) {
	call := evaluatorCall{dir: dir, function: function, run: run, files: map[string]string{}}
	for _, name := range []string{"dagger.toml", ".dagger/bench/main.go", "testdata/corpus/a.txt"} {
		if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
			call.files[name] = string(data)
		}
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	n := len(e.calls)
	reply := e.reply
	e.mu.Unlock()
	return reply(n), nil
}

func (e *evaluatorEngine) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *evaluatorEngine) recorded() []evaluatorCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

type climbFixture struct {
	*shedFixture
	engine *evaluatorEngine
	masons *intakefixture.FakeMasons
	skills *turnSkills
	jev    *gateJev
	scans  *atomic.Int64
	// prompts holds the prompt of each attempt turn and review turn by turn
	// name, and rejects the finding a reviewer turn raises, by turn name
	// prefix.
	promptMu sync.Mutex
	prompts  map[string]string
	rejects  map[string]string
}

func (c *climbFixture) recordPrompt(req agent.Request) {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	c.prompts[req.Name] = req.Prompt
}

func (c *climbFixture) promptOf(name string) (string, bool) {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	p, ok := c.prompts[name]
	return p, ok
}

func (c *climbFixture) reject(turn string) string {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	for prefix, note := range c.rejects {
		if strings.HasPrefix(turn, prefix) {
			return note
		}
	}
	return ""
}

type turnSkills struct {
	mu   sync.Mutex
	seen map[string][]string
}

func (s *turnSkills) record(name string, req agent.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string][]string{}
	}
	s.seen[name] = slices.Clone(req.Profile.Skills)
}

func (s *turnSkills) of(name string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, ok := s.seen[name]
	return got, ok
}

// newClimbFixture runs a service with a project whose dev module has no
// evaluator yet, or one that matches the climb spec, and fake agents that play
// the architect, the committee, the evaluator unit's mason and its reviewer.
func newClimbFixture(t *testing.T, engine *evaluatorEngine, evaluatorPresent bool, jevProvider *gateJev) *climbFixture {
	t.Helper()
	return newClimbFixtureWith(t, engine, evaluatorPresent, jevProvider, climbBuildingSpec)
}

func newClimbFixtureWith(t *testing.T, engine *evaluatorEngine, evaluatorPresent bool, jevProvider *gateJev, spec string) *climbFixture {
	t.Helper()
	extra := intakefixture.MasonRoles + "[kinds.hill-climb]\nskills = [\"" + climbBuildingSkill + "\"]\n"
	if jevProvider != nil {
		extra += gateJevConfig(gateKey(t, true))
	}
	scans, scanned := gateScans()
	f := newDebateFixtureWith(t, 1, 1, extra, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, config.WorkspacesGit, intakefixture.MasonCapacity(1), opts.Threads)
		opts.reviewChecks = engine
		scanned(opts)
		if jevProvider != nil {
			opts.JevProvider = jevProvider.Factory()
		}
	})
	f.engine.Mu.Lock()
	f.engine.Skills = []string{t.TempDir()}
	f.engine.Mu.Unlock()
	f.script("draft-1-1", map[string]string{kindreg.ClimbSpecPath: spec}, nil)
	f.upstream(t)
	module := climbNoEvaluator
	if evaluatorPresent {
		module = climbEvaluator
	}
	home := filepath.Dir(f.clone)
	for name, content := range map[string]string{"dagger.toml": climbDevModuleToml, ".dagger/bench/main.go": module, "testdata/corpus/a.txt": "corpus\n"} {
		path := filepath.Join(f.clone, filepath.FromSlash(name))
		fixturekit.Must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		fixturekit.Must(t, os.WriteFile(path, []byte(content), 0o600))
	}
	fixturekit.DemoGit(t, home, "-C", f.clone, "add", "--all")
	fixturekit.DemoGit(t, home, "-C", f.clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "add the dev module")
	fixturekit.DemoGit(t, home, "-C", f.clone, "push", "-q", "upstream", "HEAD:refs/heads/main")

	c := &climbFixture{shedFixture: f, engine: engine, jev: jevProvider, skills: &turnSkills{}, prompts: map[string]string{}, rejects: map[string]string{}}
	c.scans = scans
	c.masons = intakefixture.ScriptMasons(f.engine, masonTurnID)
	unit := climb.EvaluatorUnitID
	c.masons.Play[masonTurnID(unit)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		c.skills.record("evaluator-mason", req)
		path := filepath.Join(req.Workspace.Directory(), ".dagger", "bench", "main.go")
		if err := os.WriteFile(path, []byte(climbEvaluator), 0o644); err != nil {
			return err
		}
		return intakefixture.ReportDone("Wrote the evaluator")(ctx, req, tools)
	}
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID(unit)] = c.masons.Turn
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, "reviewer-climb-attempt-") {
			c.recordPrompt(req)
			verdict := map[string]any{"decision": "satisfactory", "summary": "The candidate is an honest improvement", "findings": []api.ReviewFinding{}}
			if note := c.reject(req.Name); note != "" {
				verdict = map[string]any{"decision": "material_findings", "summary": "The candidate games the metric", "findings": []api.ReviewFinding{{Severity: "high", Evidence: note, Action: "try an honest change"}}}
			}
			body, err := fixturekit.CallTool(ctx, tools, "verdict", verdict)
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if strings.HasPrefix(req.Name, intake.ReviewerAgent(unit)+"-review-") {
			c.skills.record("evaluator-reviewer", req)
			body, err := fixturekit.CallTool(ctx, tools, "verdict", map[string]any{"decision": "satisfactory", "summary": "The evaluator does the task and its acceptance holds", "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return c.masons.Chief.Turn(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()
	return c
}

// ratified hands in a hill-climb workstream, has its climb spec drafted and
// debated once, ratifies it and waits for it to be building.
func (c *climbFixture) ratified(t *testing.T) config.WorkstreamID {
	t.Helper()
	ctx := context.Background()
	faults := &intakefixture.Faults{}
	c.member(1, 1, 1, intakefixture.Concedes(faults))
	brief := "# Faster parsing\n\nMake the parser faster on the corpus.\n"
	out, err := c.c.HandIn(ctx, api.HandInRequest{Project: c.project, Key: "climb", Stdin: &brief, Kind: kindreg.HillClimb})
	fixturekit.Must(t, err)
	stream := out.Workstream
	c.awaitShed(t, stream, "concluded-1")
	faults.Check(t)
	feature := func() string {
		state, err := c.repository().Workflow(stream, trace.FeatureSubject)
		fixturekit.Must(t, err)
		return state.Value
	}
	if c.jev == nil {
		_, err = c.c.Ratify(ctx, stream, 1, 0)
		fixturekit.Must(t, err)
	} else {
		// Jev may ratify first; the owner's own ratification then finds it done.
		intakefixture.Soon(t, "the workstream to be ratified", func() bool {
			if feature() == intake.BuildingState || feature() == intake.RatifiedState {
				return true
			}
			_, err := c.c.Ratify(ctx, stream, 1, 0)
			return err == nil
		})
	}
	intakefixture.Soon(t, "the workstream to be building", func() bool { return feature() == intake.BuildingState })
	return stream
}

func (c *climbFixture) inner(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	state, err := climb.ReadInner(c.repository(), stream)
	fixturekit.Must(t, err)
	return state.Value
}

func (c *climbFixture) awaitInner(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("the climb to be %s (it is %s)", want, c.inner(t, stream)), func() bool { return c.inner(t, stream) == want })
}

func (c *climbFixture) card(t *testing.T) (api.InboxEntry, bool) {
	t.Helper()
	inbox, err := c.c.Inbox(context.Background())
	fixturekit.Must(t, err)
	for _, e := range inbox.Entries {
		if e.Kind == api.InboxClimbApproval {
			return e, true
		}
	}
	return api.InboxEntry{}, false
}

func (c *climbFixture) chiefNotices(t *testing.T, stream config.WorkstreamID, kind string) []string {
	t.Helper()
	outbox, err := c.repository().Outbox(stream)
	fixturekit.Must(t, err)
	var out []string
	for _, e := range outbox {
		if e.Event.Kind == kind {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

func (c *climbFixture) restart(t *testing.T) {
	t.Helper()
	c.stop(t)
	c.start(t)
}

func TestTheEvaluatorUnitRunsAsANormalUnitAndLandsAsTheFirstCommitThenTheOwnerApproves(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101)
	c := newClimbFixture(t, engine, false, nil)
	defer c.stop(t)
	stream := c.ratified(t)
	ctx := context.Background()

	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	c.masons.Check(t)

	// The evaluator unit went through the mason, the checks, the reviewer and
	// landing like any unit.
	for _, state := range []string{intake.UnitReady, intake.UnitImplementing, intake.UnitChecking, intake.UnitReviewing, intake.UnitApproved, intake.UnitMerged} {
		found := false
		transitions, err := trace.Read[trace.Transition](c.repository(), stream)
		fixturekit.Must(t, err)
		for _, tr := range transitions {
			found = found || tr.Subject == trace.UnitSubject(climb.EvaluatorUnitID) && tr.To == state
		}
		if !found {
			t.Errorf("the evaluator unit never was %s", state)
		}
	}
	branch := intake.FeatureBranch(stream)
	base := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(c.clone), "-C", c.clone, "rev-parse", "main"))
	commits := intakefixture.LandedCommits(t, filepath.Dir(c.clone), c.clone, stream, base)
	if len(commits) != 1 {
		t.Fatalf("the climb branch gained %d commits: %v", len(commits), commits)
	}
	evaluator := commits[0]
	if parent := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(c.clone), "-C", c.clone, "rev-parse", evaluator+"^")); parent != base {
		t.Fatalf("the evaluator commit's parent %s is not the base %s", parent, base)
	}
	if tip := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(c.clone), "-C", c.clone, "rev-parse", branch)); tip != evaluator {
		t.Fatalf("the climb branch is at %s, not at the evaluator commit %s", tip, evaluator)
	}
	for turn, want := range map[string]bool{"evaluator-mason": true, "evaluator-reviewer": true} {
		if got, ok := c.skills.of(turn); !ok || slices.Contains(got, climbBuildingSkill) != want {
			t.Errorf("%s carried skills %v (seen %v)", turn, got, ok)
		}
	}

	// Calibration ran the evaluator the configured number of times on the
	// evaluator commit, with a fresh run value each time.
	calls := engine.recorded()
	if len(calls) != 3 {
		t.Fatalf("calibration called the evaluator %d times, want 3", len(calls))
	}
	runs := map[string]bool{}
	for _, call := range calls {
		runs[call.run] = true
		if call.function != "parse-bench" || !strings.Contains(call.files[".dagger/bench/main.go"], "ParseBench") || call.files["dagger.toml"] != climbDevModuleToml || call.files["testdata/corpus/a.txt"] != "corpus\n" {
			t.Errorf("a calibration run saw %+v", call)
		}
	}
	if len(runs) != 3 {
		t.Fatalf("the calibration runs passed %d distinct run values", len(runs))
	}
	cal, _, found, err := climb.ReadCalibration(c.repository(), stream)
	fixturekit.Must(t, err)
	if !found || cal.Commit != evaluator || cal.Calibration.Baseline != 101 || cal.Calibration.Spread != 3 || cal.Calibration.Verdict != climb.Calibrated || cal.Calibration.Units["wall_ms"] != "ms" {
		t.Fatalf("calibration %+v", cal)
	}

	// The owner's card shows the diff, the protected paths, the baseline and
	// the spread, and no attempt starts before the owner approves it.
	card, open := c.card(t)
	if !open {
		t.Fatal("the evaluator approval card is not in the inbox")
	}
	for _, want := range []string{"parse-bench", evaluator, "ParseBench", "baseline is 101 ms", "spread", "3 ms", ".dagger/bench", "dagger.toml", "dagger.lock", "testdata/corpus"} {
		if !strings.Contains(card.Question, want) {
			t.Errorf("the card lacks %q:\n%s", want, card.Question)
		}
	}
	if card.Workstream != stream || !slices.Equal(card.Options, []string{"approve"}) || card.Answer.Path != api.Prefix+"/climb/"+string(stream)+"/approval" || card.Answer.Body["commit"] != evaluator || card.Jev != nil {
		t.Fatalf("the card %+v", card)
	}
	if _, ok, err := climb.ReadApproval(c.repository(), stream); err != nil || ok {
		t.Fatalf("an approval is recorded before the owner approved: %v %v", ok, err)
	}
	if _, err := c.c.ApproveClimb(ctx, stream, api.ClimbApprovalRequest{Decision: "approve", Commit: "stale"}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("approving another commit: %v", err)
	}
	if _, err := c.c.ApproveClimb(ctx, stream, api.ClimbApprovalRequest{Decision: "decline", Commit: evaluator}); !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("a decision the card does not offer: %v", err)
	}

	// A restart while the card is open resumes in the same state and neither
	// calibrates again nor approves.
	c.restart(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	awaitGateScans(t, c.scans)
	if engine.count() != 3 || c.inner(t, stream) != climb.InnerAwaitingApproval {
		t.Fatalf("after the restart: %s, %d evaluator calls", c.inner(t, stream), engine.count())
	}
	if _, open := c.card(t); !open {
		t.Fatal("the card did not survive the restart")
	}

	out, err := c.c.ApproveClimb(ctx, stream, api.ClimbApprovalRequest{Decision: "approve", Commit: evaluator})
	fixturekit.Must(t, err)
	if out.Commit != evaluator || out.Baseline != 101 || out.Noise != 3 || !slices.Contains(out.Protected, "testdata/corpus") {
		t.Fatalf("approval %+v", out)
	}
	if c.inner(t, stream) != climb.InnerClimbing {
		t.Fatalf("inner state %q after the owner's approval", c.inner(t, stream))
	}
	approval, ok, err := climb.ReadApproval(c.repository(), stream)
	fixturekit.Must(t, err)
	files := map[string]string{}
	for _, f := range approval.Approved.Files {
		files[f.Path] = string(f.Content)
	}
	if !ok || approval.Commit != evaluator || approval.ApprovedBy != "owner" || files["testdata/corpus/a.txt"] != "corpus\n" || files["dagger.toml"] != climbDevModuleToml || !strings.Contains(files[".dagger/bench/main.go"], "ParseBench") {
		t.Fatalf("approval record %+v with files %v", approval, files)
	}
	if _, open := c.card(t); open {
		t.Fatal("the card is still open after the approval")
	}

	c.restart(t)
	c.awaitInner(t, stream, climb.InnerClimbing)
	if engine.count() != 3 {
		t.Fatalf("a restart while climbing called the evaluator %d times", engine.count())
	}
}

func TestAMatchingEvaluatorIsReusedAndTheUnitIsSkipped(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 101, 100)
	c := newClimbFixture(t, engine, true, nil)
	defer c.stop(t)
	stream := c.ratified(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	rec, found, err := climb.ReadEvaluator(c.repository(), stream)
	fixturekit.Must(t, err)
	if !found || !rec.Skipped {
		t.Fatalf("evaluator record %+v", rec)
	}
	if state, err := c.repository().Workflow(stream, trace.UnitSubject(climb.EvaluatorUnitID)); err != nil || state.Value != "" {
		t.Fatalf("a skipped evaluator unit has state %+v %v", state, err)
	}
	if runs := c.masons.Requests(stream); len(runs) != 0 {
		t.Fatalf("a mason ran %d turns although the unit was skipped", len(runs))
	}
	base := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(c.clone), "-C", c.clone, "rev-parse", "main"))
	if commits := intakefixture.LandedCommits(t, filepath.Dir(c.clone), c.clone, stream, base); len(commits) != 0 {
		t.Fatalf("the climb branch gained commits %v", commits)
	}
	if engine.count() != 3 {
		t.Fatalf("calibration called the evaluator %d times", engine.count())
	}
	card, open := c.card(t)
	if !open || !strings.Contains(card.Question, "Evaluator diff: none") || card.Answer.Body["commit"] != base {
		t.Fatalf("the card %+v", card)
	}
}

func TestACalibrationThatFindsTheMetricBrokenOrNoisyHoldsTheClimb(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		engine *evaluatorEngine
		state  string
		notice string
	}{
		"an invalid run": {func() *evaluatorEngine {
			e := newEvaluatorEngine()
			e.reply = func(n int) checks.CallResult {
				if n == 2 {
					return checks.CallResult{Stdout: `{"schema":1,"valid":false,"reason":"output differs from the oracle","metrics":{}}`}
				}
				return checks.CallResult{Stdout: benchOutput(100)}
			}
			return e
		}(), climb.InnerHeldBroken, "the metric, not the code, is broken"},
		"a run that lacks the primary metric": {func() *evaluatorEngine {
			e := newEvaluatorEngine()
			e.reply = func(int) checks.CallResult {
				return checks.CallResult{Stdout: `{"schema":1,"valid":true,"reason":"ok","metrics":{"peak_rss_mb":{"value":1,"unit":"MB","direction":"lower"}}}`}
			}
			return e
		}(), climb.InnerHeldBroken, "the metric, not the code, is broken"},
		"a spread at the minimum improvement": {newEvaluatorEngine(100, 105, 100), climb.InnerHeldNoisy, "too noisy to climb"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClimbFixture(t, tc.engine, true, nil)
			defer c.stop(t)
			stream := c.ratified(t)
			c.awaitInner(t, stream, tc.state)
			notices := c.chiefNotices(t, stream, trace.NoticeKind)
			held := ""
			for _, n := range notices {
				if strings.Contains(n, tc.notice) {
					held = n
				}
			}
			if held == "" {
				t.Fatalf("the chief of staff was not told %q: %v", tc.notice, notices)
			}
			if _, open := c.card(t); open {
				t.Fatal("a held climb opened the approval card")
			}
			calls := tc.engine.count()
			c.restart(t)
			c.awaitInner(t, stream, tc.state)
			awaitGateScans(t, c.scans)
			if tc.engine.count() != calls || c.inner(t, stream) != tc.state {
				t.Fatalf("a restart of a held climb: %s after %d calls, was %d", c.inner(t, stream), tc.engine.count(), calls)
			}
			if _, err := c.c.ApproveClimb(context.Background(), stream, api.ClimbApprovalRequest{Decision: "approve", Commit: "any"}); !intakefixture.Failed(err, api.Conflict) {
				t.Fatalf("approving a held climb: %v", err)
			}
		})
	}
}

func TestJevNeverApprovesTheEvaluatorEvenWhenItDecidesOwnerGatesAndIsConfident(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 101, 100)
	always := &gateJev{confidence: 0.999, pick: func(_ string, options []string) string {
		for _, want := range []string{"approve", "ratify"} {
			if slices.Contains(options, want) {
				return want
			}
		}
		return options[0]
	}}
	c := newClimbFixture(t, engine, true, always)
	defer c.stop(t)
	stream := c.ratified(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	for i := 0; i < 3; i++ {
		awaitGateScans(t, c.scans)
	}
	card, open := c.card(t)
	if !open || card.Jev != nil {
		t.Fatalf("the evaluator card with Jev configured: open %v, %+v", open, card)
	}
	if c.inner(t, stream) != climb.InnerAwaitingApproval {
		t.Fatalf("inner state %q: a confident Jev approval started the climb", c.inner(t, stream))
	}
	if _, ok, err := climb.ReadApproval(c.repository(), stream); err != nil || ok {
		t.Fatalf("an approval is recorded without the owner: %v %v", ok, err)
	}
	for _, r := range always.Requests() {
		state, _ := r.State.(string)
		if strings.Contains(state, "Approve the evaluator") || strings.Contains(state, "Evaluator diff") {
			t.Fatalf("Jev was asked about the evaluator approval:\n%s", state)
		}
	}
	judged, _ := judgments(t, c.repository(), stream)
	for _, r := range judged {
		if r.Occurrence != nil && r.Occurrence.Kind == string(jev.KindClimbApproval) {
			t.Fatalf("a judgment of the evaluator approval is recorded: %+v", r)
		}
	}

	out, err := c.c.ApproveClimb(context.Background(), stream, api.ClimbApprovalRequest{Decision: "approve", Commit: card.Answer.Body["commit"].(string)})
	fixturekit.Must(t, err)
	if c.inner(t, stream) != climb.InnerClimbing || out.Workstream != stream {
		t.Fatalf("after the owner's approval: %s %+v", c.inner(t, stream), out)
	}
}

func TestTheGateWorkerNeverJudgesTheClimbApproval(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.999}
	f := newGateFixture(t, p)
	g := f.gate(jev.KindClimbApproval, "climb:evaluator", "approve")
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 0 || len(p.Requests()) != 0 {
		t.Fatalf("the owner's own card was applied %d times after %d requests to Jev", g.applied.Load(), len(p.Requests()))
	}
	if _, err := f.s.jev.Lookup(f.repo, g.gate.Stream, g.gate.Kind, g.gate.Key); err == nil {
		t.Fatal("Jev holds a judgment record for the owner's own card")
	}
}
