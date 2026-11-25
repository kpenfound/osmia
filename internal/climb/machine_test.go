package climb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const evaluatorSource = "package eval\n\nfunc (m *Eval) MeasureWall(run string) string { return measure(run, \"wall_ms\") }\n"

const evalToml = "[modules.eval]\nsource = \".dagger/eval\"\nmain = true\n"

type fakeClone struct {
	mu       sync.Mutex
	files    map[string]string
	base     string
	diff     string
	branches map[string]string
	exports  []string
}

func (c *fakeClone) Export(_ context.Context, commit, dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exports = append(c.exports, commit)
	for name, content := range c.files {
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeClone) Branch(_ context.Context, name string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	commit, ok := c.branches[name]
	return commit, ok, nil
}

func (c *fakeClone) Diff(_ context.Context, base, candidate string) (string, error) {
	return c.diff, nil
}

func (c *fakeClone) land(commit, diff string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.branches["osmia/"+string(fixturekit.Stream)] = commit
	c.diff = diff
}

type machineRig struct {
	t        *testing.T
	repo     *trace.Repository
	clone    *fakeClone
	engine   *fakeEngine
	slots    *fakeSlots
	revision int
	machine  *Machine
	steps    []string
}

func climbSpec() Spec {
	return Spec{Version: SpecVersion, Goal: "Make the parser faster.", Evaluator: "measure-wall", Repeats: 1,
		Metric:     MetricSpec{Name: "wall_ms", Direction: Lower, MinImprovement: 5},
		Guardrails: GuardrailSpec{Limits: []SecondaryLimit{{Metric: "peak_rss_mb", Direction: Lower, Max: ptr(200.0)}}},
		Protected:  Protected{Fixtures: []string{"fixtures"}},
		Stop:       StopRules{MaxAttempts: 10}}
}

func newMachineRig(t *testing.T, evaluatorPresent bool, replies ...string) *machineRig {
	t.Helper()
	repo := newAttemptStore(t)
	ctx := context.Background()
	h := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
	}
	if _, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("fixture-building"), Subject: trace.FeatureSubject, To: "building", Reason: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"dagger.toml": evalToml, "dagger.lock": "lock\n", "fixtures/input.txt": "input\n", "src/work.go": "package src\n"}
	if evaluatorPresent {
		files[".dagger/eval/main.go"] = evaluatorSource + "// reports wall_ms\n"
	} else {
		files[".dagger/eval/main.go"] = "package eval\n"
	}
	branch := "osmia/" + string(fixturekit.Stream)
	clone := &fakeClone{files: files, branches: map[string]string{branch: "base0001"}, base: "base0001"}
	slots := &fakeSlots{}
	engine := &fakeEngine{slots: slots}
	r := &machineRig{t: t, repo: repo, clone: clone, engine: engine, slots: slots, revision: 1}
	r.reply(replies...)
	r.machine = &Machine{
		Repository: repo,
		Spec:       func(config.WorkstreamID) (Spec, int, error) { return climbSpec(), r.revision, nil },
		Settings:   func() kinds.HillClimbSettings { return kinds.HillClimbSettings{CalibrationRuns: 3, Repeats: 1} },
		Workspace:  func(config.WorkstreamID) (Workspace, error) { return clone, nil },
		Branch:     func(stream config.WorkstreamID) string { return "osmia/" + string(stream) },
		Runner: func(_ config.WorkstreamID, source Source) (Runner, error) {
			return Runner{Source: source, Engine: engine, AcquireSlot: slots.acquire, Root: filepath.Join(t.TempDir(), "measure"), Timeout: time.Minute}, nil
		},
		Root: filepath.Join(t.TempDir(), "export"),
		Now:  func() time.Time { return fixturekit.DemoStart },
		Step: func(name string) error { r.steps = append(r.steps, name); return nil },
	}
	return r
}

func (r *machineRig) reply(replies ...string) {
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	r.engine.calls = nil
	r.engine.reply = func(n int) (checks.CallResult, error) {
		if n > len(replies) {
			return checks.CallResult{}, fmt.Errorf("unexpected call %d", n)
		}
		return ok(replies[n-1])
	}
}

func (r *machineRig) reconcile() {
	r.t.Helper()
	if err := r.machine.Reconcile(context.Background(), fixturekit.Stream); err != nil {
		r.t.Fatal(err)
	}
}

func (r *machineRig) inner() string {
	r.t.Helper()
	state, err := ReadInner(r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	return state.Value
}

func (r *machineRig) calls() int {
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	return len(r.engine.calls)
}

func (r *machineRig) notices(kind string) []string {
	r.t.Helper()
	outbox, err := r.repo.Outbox(fixturekit.Stream)
	fixturekit.Must(r.t, err)
	var out []string
	for _, e := range outbox {
		if e.Event.Kind == kind {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

func (r *machineRig) mergeEvaluatorUnit() {
	r.t.Helper()
	subject := trace.UnitSubject(EvaluatorUnitID)
	state, err := r.repo.Workflow(fixturekit.Stream, subject)
	fixturekit.Must(r.t, err)
	if state.Value != "ready" {
		r.t.Fatalf("the evaluator unit is %q, want ready", state.Value)
	}
	for i, to := range []string{"implementing", "merged"} {
		from := []string{"ready", "implementing"}[i]
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: subject + "-" + to, Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
		_, err := r.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version + uint64(i), Transition: trace.Transition{Header: h, Subject: subject, From: from, To: to, Reason: "fixture"}})
		fixturekit.Must(r.t, err)
	}
	r.clone.land("eval0001", "diff --git a/.dagger/eval/main.go b/.dagger/eval/main.go\n+func MeasureWall\n")
}

func scored(wall float64) string { return output(wall, 120) }

func TestTheEvaluatorUnitRunsWhenTheProjectHasNoMatchingFunction(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, false)
	r.reconcile()
	if got := r.inner(); got != InnerEvaluator {
		t.Fatalf("inner state %q, want %q", got, InnerEvaluator)
	}
	unit, err := r.repo.Workflow(fixturekit.Stream, trace.UnitSubject(EvaluatorUnitID))
	fixturekit.Must(t, err)
	if unit.Value != "ready" {
		t.Fatalf("the evaluator unit is %q, want ready", unit.Value)
	}
	rec, found, err := ReadEvaluator(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !found || rec.Skipped || rec.Base != "base0001" || rec.Module != ".dagger/eval" || !strings.Contains(rec.Evidence, "measure-wall") {
		t.Fatalf("evaluator record %+v", rec)
	}
	r.reconcile()
	if got := r.inner(); got != InnerEvaluator || r.calls() != 0 {
		t.Fatalf("a second pass moved the climb to %q after %d calls", got, r.calls())
	}
}

func TestTheEvaluatorUnitIsSkippedWhenAMatchingFunctionExists(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(101), scored(100))
	r.reconcile()
	if got := r.inner(); got != InnerCalibrating {
		t.Fatalf("inner state %q, want %q", got, InnerCalibrating)
	}
	if unit, err := r.repo.Workflow(fixturekit.Stream, trace.UnitSubject(EvaluatorUnitID)); err != nil || unit.Value != "" {
		t.Fatalf("a skipped evaluator unit has state %+v %v", unit, err)
	}
	rec, _, err := ReadEvaluator(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !rec.Skipped {
		t.Fatalf("evaluator record %+v", rec)
	}
	r.reconcile()
	if got := r.inner(); got != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("calibration against the reused function left %q after %d calls", got, r.calls())
	}
	cal, _, found, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !found || cal.Commit != "base0001" || cal.Diff != "" || cal.Calibration.Verdict != Calibrated {
		t.Fatalf("calibration %+v", cal)
	}
}

func TestCalibrationRunsTheConfiguredCountAndRecordsBaselineAndSpread(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, false, scored(100), scored(103), scored(101))
	r.reconcile()
	r.mergeEvaluatorUnit()
	r.reconcile()
	if got := r.inner(); got != InnerCalibrating {
		t.Fatalf("inner state %q after the evaluator unit landed, want %q", got, InnerCalibrating)
	}
	r.reconcile()
	if got := r.inner(); got != InnerAwaitingApproval {
		t.Fatalf("inner state %q, want %q", got, InnerAwaitingApproval)
	}
	cal, _, found, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	c := cal.Calibration
	if !found || cal.Commit != "eval0001" || cal.Base != "base0001" || !strings.Contains(cal.Diff, "MeasureWall") || c.Baseline != 101 || c.Spread != 3 || c.Units["wall_ms"] != "ms" || len(c.Measurement.Runs) != 3 || cal.SpecRevision != 1 {
		t.Fatalf("calibration %+v", cal)
	}
	for _, want := range []string{".dagger/eval", "dagger.lock", "dagger.toml", "fixtures"} {
		if !slices.Contains(cal.Protected, want) {
			t.Errorf("protected paths %v lack %s", cal.Protected, want)
		}
	}
	runs := map[string]bool{}
	r.engine.mu.Lock()
	for _, c := range r.engine.calls {
		runs[c.run] = true
		if c.function != "measure-wall" {
			t.Errorf("called %q", c.function)
		}
	}
	r.engine.mu.Unlock()
	if len(runs) != 3 {
		t.Fatalf("the runs passed %d distinct run values", len(runs))
	}
	if progress := r.notices(trace.ProgressKind); !slices.ContainsFunc(progress, func(body string) bool { return strings.Contains(body, "baseline 101 ms, spread 3") }) {
		t.Fatalf("progress to the chief: %v", progress)
	}
	if len(r.notices(trace.NoticeKind)) != 0 {
		t.Fatalf("a calibrated climb raised notices %v", r.notices(trace.NoticeKind))
	}
}

func TestAFailedOrInvalidRunHoldsTheClimbAsABrokenMetric(t *testing.T) {
	t.Parallel()
	missing := `{"schema":1,"valid":true,"reason":"ok","metrics":{"peak_rss_mb":{"value":1,"unit":"MB","direction":"lower"}}}`
	invalid := `{"schema":1,"valid":false,"reason":"the output differs from the oracle","metrics":{}}`
	for name, replies := range map[string][]string{
		"an invalid run":                     {scored(100), invalid, scored(100)},
		"a run without the primary metric":   {scored(100), missing, scored(100)},
		"output that is not the contract":    {scored(100), "not json", scored(100)},
		"a run whose unit differs":           {scored(100), strings.ReplaceAll(scored(100), `"unit":"ms"`, `"unit":"s"`), scored(100)},
		"a run whose direction differs":      {scored(100), strings.ReplaceAll(scored(100), `"direction":"lower"`, `"direction":"higher"`), scored(100)},
		"the first run failing":              {invalid},
		"a secondary metric the limit needs": {scored(100), `{"schema":1,"valid":true,"reason":"ok","metrics":{"wall_ms":{"value":1,"unit":"ms","direction":"lower"}}}`, scored(100)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newMachineRig(t, true, replies...)
			r.reconcile()
			r.reconcile()
			if got := r.inner(); got != InnerHeldBroken {
				t.Fatalf("inner state %q, want %q", got, InnerHeldBroken)
			}
			notices := r.notices(trace.NoticeKind)
			if len(notices) != 1 || !strings.Contains(notices[0], "the metric, not the code, is broken") {
				t.Fatalf("notices to the chief: %v", notices)
			}
			cal, _, found, err := ReadCalibration(r.repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if !found || cal.Calibration.Verdict != BrokenMetric || cal.Calibration.Baseline != 0 {
				t.Fatalf("calibration %+v", cal.Calibration)
			}
			held := r.calls()
			r.reconcile()
			r.reconcile()
			if r.inner() != InnerHeldBroken || r.calls() != held || len(r.notices(trace.NoticeKind)) != 1 {
				t.Fatalf("a held climb ran again: %d calls, %d notices", r.calls(), len(r.notices(trace.NoticeKind)))
			}
			if _, err := r.machine.Approve(context.Background(), fixturekit.Stream, "base0001", trace.Actor{Kind: "owner", ID: "local"}); !errors.Is(err, ErrNotAwaiting) {
				t.Fatalf("approving a held climb: %v", err)
			}
		})
	}
}

func TestASpreadAtTheMinimumImprovementHoldsTheClimbAsTooNoisy(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(105), scored(100))
	r.reconcile()
	r.reconcile()
	if got := r.inner(); got != InnerHeldNoisy {
		t.Fatalf("inner state %q, want %q", got, InnerHeldNoisy)
	}
	notices := r.notices(trace.NoticeKind)
	if len(notices) != 1 || !strings.Contains(notices[0], "too noisy to climb") {
		t.Fatalf("notices to the chief: %v", notices)
	}
	cal, _, _, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if cal.Calibration.Verdict != TooNoisy || cal.Calibration.Spread != 5 {
		t.Fatalf("calibration %+v", cal.Calibration)
	}
}

func TestOnlyAnApprovalPinningTheCalibratedCommitStartsClimbing(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(101), scored(100))
	owner := trace.Actor{Kind: "owner", ID: "local"}
	ctx := context.Background()
	if _, err := r.machine.Approve(ctx, fixturekit.Stream, "base0001", owner); !errors.Is(err, ErrNotAwaiting) {
		t.Fatalf("approving before calibration: %v", err)
	}
	r.reconcile()
	r.reconcile()
	if r.inner() != InnerAwaitingApproval {
		t.Fatalf("inner state %q", r.inner())
	}
	if _, ok, err := ReadApproval(r.repo, fixturekit.Stream); err != nil || ok {
		t.Fatalf("an approval is recorded before the owner approved: %v %v", ok, err)
	}
	r.reconcile()
	if r.inner() != InnerAwaitingApproval {
		t.Fatalf("a pass moved an unapproved climb to %q", r.inner())
	}
	if _, err := r.machine.Approve(ctx, fixturekit.Stream, "other", owner); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("approving another commit: %v", err)
	}
	r.clone.land("moved002", "")
	if _, err := r.machine.Approve(ctx, fixturekit.Stream, "base0001", owner); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("approving after the branch moved: %v", err)
	}
	if r.inner() != InnerAwaitingApproval {
		t.Fatalf("a refused approval moved the climb to %q", r.inner())
	}
	r.clone.land("base0001", "")

	// The attempt-free tree the owner approved is what is frozen, whatever the
	// working tree holds later.
	approved, err := r.machine.Approve(ctx, fixturekit.Stream, "base0001", owner)
	fixturekit.Must(t, err)
	if r.inner() != InnerClimbing {
		t.Fatalf("inner state %q after approval", r.inner())
	}
	rec, ok, err := ReadApproval(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !ok || rec.Commit != "base0001" || rec.Baseline != 100 || rec.Noise != 1 || rec.Units["wall_ms"] != "ms" || rec.Evaluator != "measure-wall" || rec.ApprovedBy != "owner" {
		t.Fatalf("approval %+v", rec)
	}
	if !slices.Equal(rec.Protected, approved.Protected) || !slices.Equal(rec.Approved.Paths, rec.Protected) {
		t.Fatalf("the frozen copy covers %v, not %v", rec.Approved.Paths, rec.Protected)
	}
	files := map[string]string{}
	for _, f := range rec.Approved.Files {
		files[f.Path] = string(f.Content)
	}
	if files["dagger.toml"] != evalToml || files["fixtures/input.txt"] != "input\n" || files[".dagger/eval/main.go"] == "" || files["src/work.go"] != "" {
		t.Fatalf("the frozen files %v", files)
	}
	again, err := r.machine.Approve(ctx, fixturekit.Stream, "base0001", owner)
	fixturekit.Must(t, err)
	if again.Commit != rec.Commit || r.inner() != InnerClimbing {
		t.Fatalf("a second approval: %+v %q", again, r.inner())
	}
	r.reconcile()
	if r.inner() != InnerClimbing || r.calls() != 3 {
		t.Fatalf("climbing resumed as %q after %d calls", r.inner(), r.calls())
	}
}

func TestARestartInEachInnerStateRepeatsNoRecordedSideEffect(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, false, scored(100), scored(101), scored(100))
	fresh := func() {
		clone := r.machine
		next := *clone
		r.machine = &next
	}
	r.reconcile()
	fresh()
	r.reconcile()
	units, err := trace.Read[trace.Transition](r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	count := func(id string) int {
		n := 0
		for _, tr := range units {
			if tr.ID == id {
				n++
			}
		}
		return n
	}
	if count("climb-start") != 1 || count("unit-evaluator-planned") != 1 || count("unit-evaluator-ready") != 1 {
		t.Fatalf("the evaluator step repeated: %d %d %d", count("climb-start"), count("unit-evaluator-planned"), count("unit-evaluator-ready"))
	}
	r.mergeEvaluatorUnit()
	r.reconcile()
	fresh()
	r.reconcile()
	fresh()
	r.reconcile()
	if r.inner() != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("inner state %q after %d calls", r.inner(), r.calls())
	}
	fresh()
	r.reconcile()
	if r.inner() != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("a restart while awaiting approval ran calibration again: %q after %d calls", r.inner(), r.calls())
	}
	if _, err := r.machine.Approve(context.Background(), fixturekit.Stream, "eval0001", trace.Actor{Kind: "owner", ID: "local"}); err != nil {
		t.Fatal(err)
	}
	fresh()
	r.reconcile()
	if r.inner() != InnerClimbing || r.calls() != 3 {
		t.Fatalf("a restart while climbing left %q after %d calls", r.inner(), r.calls())
	}
}

func TestACalibrationThatCannotRunRecordsNothingAndRunsAgain(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true)
	r.engine.mu.Lock()
	r.engine.reply = func(int) (checks.CallResult, error) { return checks.CallResult{}, errors.New("the engine is down") }
	r.engine.mu.Unlock()
	r.reconcile()
	if err := r.machine.Reconcile(context.Background(), fixturekit.Stream); err == nil {
		t.Fatal("a calibration the engine could not run was not reported")
	}
	if r.inner() != InnerCalibrating {
		t.Fatalf("inner state %q, want calibrating", r.inner())
	}
	if _, _, found, _ := ReadCalibration(r.repo, fixturekit.Stream); found {
		t.Fatal("a calibration that did not run was recorded")
	}
	r.reply(scored(100), scored(100), scored(101))
	r.reconcile()
	if r.inner() != InnerAwaitingApproval {
		t.Fatalf("inner state %q, want awaiting-approval", r.inner())
	}
}

func TestAHeldClimbCalibratesAgainWhenItsClimbSpecChanges(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(120), scored(100))
	r.reconcile()
	r.reconcile()
	if r.inner() != InnerHeldNoisy {
		t.Fatalf("inner state %q", r.inner())
	}
	r.reply(scored(100), scored(101), scored(100))
	r.reconcile()
	if r.inner() != InnerHeldNoisy || r.calls() != 0 {
		t.Fatalf("an unchanged climb spec calibrated again: %q after %d calls", r.inner(), r.calls())
	}
	r.revision = 2
	r.reconcile()
	r.reconcile()
	if r.inner() != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("inner state %q after %d calls", r.inner(), r.calls())
	}
	cal, _, _, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if cal.SpecRevision != 2 || cal.Calibration.Verdict != Calibrated {
		t.Fatalf("calibration %+v", cal)
	}
}

func TestFindEvaluatorMatchesTheFunctionTheClimbSpecNames(t *testing.T) {
	t.Parallel()
	spec := climbSpec()
	write := func(files map[string]string) string {
		dir := t.TempDir()
		for name, content := range files {
			fixturekit.Must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
			fixturekit.Must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		}
		return dir
	}
	for name, tc := range map[string]struct {
		files map[string]string
		want  bool
	}{
		"camel case":             {map[string]string{"main.go": "func (m *M) MeasureWall(run string) string { return \"wall_ms\" }"}, true},
		"snake case":             {map[string]string{"main.py": "def measure_wall(run: str) -> str:\n    return 'wall_ms'"}, true},
		"kebab case in a script": {map[string]string{"main.dang": "pub measure-wall(run: String!): String! { \"wall_ms\" }"}, true},
		"another function":       {map[string]string{"main.go": "func (m *M) MeasureWallClock(run string) string { return \"wall_ms\" }"}, false},
		"no run argument":        {map[string]string{"main.go": "func (m *M) MeasureWall() string { return \"wall_ms\" }"}, false},
		"another metric":         {map[string]string{"main.go": "func (m *M) MeasureWall(run string) string { return \"cpu\" }"}, false},
		"nothing":                {map[string]string{"main.go": "package eval"}, false},
		"generated sdk only":     {map[string]string{"sdk/gen.go": "func MeasureWall(run string) string { return \"wall_ms\" }"}, false},
	} {
		found, evidence, err := FindEvaluator(write(tc.files), spec)
		fixturekit.Must(t, err)
		if found != tc.want || evidence == "" {
			t.Errorf("%s: found %v (%s), want %v", name, found, evidence, tc.want)
		}
	}
}

func TestTheEvaluatorUnitIsMadeFromTheClimbSpec(t *testing.T) {
	t.Parallel()
	u := EvaluatorUnit(climbSpec())
	if u.ID != "evaluator" || len(u.DependsOn) != 0 || len(u.Criteria) != 0 || len(u.Acceptance) == 0 || !slices.Equal(u.Footprint, []string{"**"}) {
		t.Fatalf("unit %+v", u)
	}
	for _, want := range []string{"measure-wall", "run: String!", "wall_ms", "peak_rss_mb", "schema", "not a @check", "hill-climb-evaluator"} {
		if !strings.Contains(u.Task, want) {
			t.Errorf("the task lacks %q:\n%s", want, u.Task)
		}
	}
	if p := EvaluatorPlan(climbSpec()); len(p.Units) != 1 || p.Units[0].ID != EvaluatorUnitID {
		t.Fatalf("plan %+v", p)
	}
}
