package climb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
)

type engineCall struct {
	dir, function, run string
	files              map[string]string
	slotsHeld          int
}

type fakeEngine struct {
	mu      sync.Mutex
	calls   []engineCall
	slots   *fakeSlots
	reply   func(n int) (checks.CallResult, error)
	inspect []string
	block   bool
}

func (e *fakeEngine) Call(ctx context.Context, dir, function, run string) (checks.CallResult, error) {
	e.mu.Lock()
	call := engineCall{dir: dir, function: function, run: run, files: map[string]string{}, slotsHeld: e.slots.heldNow()}
	for _, name := range e.inspect {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			call.files[name] = string(data)
		}
	}
	e.calls = append(e.calls, call)
	n := len(e.calls)
	e.mu.Unlock()
	if e.block {
		<-ctx.Done()
		return checks.CallResult{ExitCode: -1}, ctx.Err()
	}
	return e.reply(n)
}

type fakeSlots struct {
	mu       sync.Mutex
	held     int
	acquired int
	refuse   error
}

func (s *fakeSlots) heldNow() int { s.mu.Lock(); defer s.mu.Unlock(); return s.held }

func (s *fakeSlots) acquire(ctx context.Context) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse != nil {
		return nil, s.refuse
	}
	s.held++
	s.acquired++
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.held--; s.mu.Unlock() }) }, nil
}

type fakeSource struct {
	files   map[string]string
	exports []string
}

func (s *fakeSource) Export(_ context.Context, commit, dir string) error {
	s.exports = append(s.exports, dir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return fmt.Errorf("export target %s is not an empty directory", dir)
	}
	for name, content := range s.files {
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

func output(wall, rss float64) string {
	return fmt.Sprintf(`{"schema":1,"valid":true,"reason":"ok","metrics":{"wall_ms":{"value":%v,"unit":"ms","direction":"lower"},"peak_rss_mb":{"value":%v,"unit":"MB","direction":"lower"}}}`, wall, rss)
}

func ok(stdout string) (checks.CallResult, error) { return checks.CallResult{Stdout: stdout}, nil }

type rig struct {
	runner Runner
	engine *fakeEngine
	slots  *fakeSlots
	source *fakeSource
	req    Request
}

func newRig(t *testing.T, replies ...string) *rig {
	t.Helper()
	slots := &fakeSlots{}
	engine := &fakeEngine{slots: slots}
	engine.reply = func(n int) (checks.CallResult, error) {
		if n > len(replies) {
			return checks.CallResult{}, fmt.Errorf("unexpected call %d", n)
		}
		return ok(replies[n-1])
	}
	source := &fakeSource{files: map[string]string{
		"dagger.toml":           "tampered by the attempt\n",
		".dagger/eval/main.go":  "package eval // tampered\n",
		".dagger/eval/cheat.go": "package eval // added by the attempt\n",
		"fixtures/input.txt":    "tampered\n",
		"src/work.go":           "package src // the candidate's own change\n",
	}}
	approved := ApprovedCopy{
		Paths: []string{"dagger.toml", ".dagger/eval", "fixtures"},
		Files: []ApprovedFile{
			{Path: "dagger.toml", Mode: 0o644, Content: []byte("approved\n")},
			{Path: ".dagger/eval/main.go", Mode: 0o644, Content: []byte("package eval // approved\n")},
			{Path: "fixtures/input.txt", Mode: 0o644, Content: []byte("approved input\n")},
		},
	}
	r := &rig{engine: engine, slots: slots, source: source}
	r.runner = Runner{Source: source, Engine: engine, AcquireSlot: slots.acquire, Root: filepath.Join(t.TempDir(), "exports"), Timeout: time.Minute}
	r.req = Request{Commit: "abc123", Function: "measure-wall", Approved: approved, Expect: expectations().WithUnits(map[string]string{"wall_ms": "ms", "peak_rss_mb": "MB"}), Repeats: 1}
	return r
}

func TestRunnerPassesOnlyAFreshRandomRunValueEachRun(t *testing.T) {
	t.Parallel()
	r := newRig(t, output(100, 10), output(101, 10), output(102, 10), output(103, 10))
	r.req.Repeats = 3
	m, err := r.runner.Measure(context.Background(), r.req)
	if err != nil || !m.Valid {
		t.Fatalf("%+v, %v", m, err)
	}
	if _, err := r.runner.Measure(context.Background(), Request{Commit: r.req.Commit, Function: r.req.Function, Approved: r.req.Approved, Expect: r.req.Expect, Repeats: 1}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, c := range r.engine.calls {
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(c.run) || seen[c.run] {
			t.Fatalf("call %d run value %q (seen %v)", i, c.run, seen)
		}
		seen[c.run] = true
		if c.function != "measure-wall" {
			t.Fatalf("function %q", c.function)
		}
		if m.Runs != nil && i < 3 && m.Runs[i].Value != c.run {
			t.Fatalf("run %d records value %q, the engine got %q", i, m.Runs[i].Value, c.run)
		}
	}
	if len(r.engine.calls) != 4 {
		t.Fatalf("%d calls", len(r.engine.calls))
	}
}

func TestRunnerUsesAFreshExportWithTheProtectedPathsRestored(t *testing.T) {
	t.Parallel()
	r := newRig(t, output(100, 10), output(100, 10))
	r.req.Repeats = 2
	r.engine.inspect = []string{"dagger.toml", ".dagger/eval/main.go", ".dagger/eval/cheat.go", "fixtures/input.txt", "src/work.go"}
	if _, err := r.runner.Measure(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for i, c := range r.engine.calls {
		if dirs[c.dir] || filepath.Dir(c.dir) != r.runner.Root {
			t.Fatalf("call %d ran in %s (seen %v, root %s)", i, c.dir, dirs, r.runner.Root)
		}
		dirs[c.dir] = true
		want := map[string]string{
			"dagger.toml":          "approved\n",
			".dagger/eval/main.go": "package eval // approved\n",
			"fixtures/input.txt":   "approved input\n",
			"src/work.go":          "package src // the candidate's own change\n",
		}
		for name, content := range want {
			if c.files[name] != content {
				t.Errorf("call %d: %s is %q, want %q", i, name, c.files[name], content)
			}
		}
		if _, present := c.files[".dagger/eval/cheat.go"]; present {
			t.Errorf("call %d: a file the attempt added to a protected path survived", i)
		}
	}
	if len(r.source.exports) != 2 {
		t.Fatalf("exports %v", r.source.exports)
	}
	for _, dir := range r.source.exports {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("export %s was not removed: %v", dir, err)
		}
	}
}

func TestRunnerHoldsACheckSlotWhileTheEvaluatorRuns(t *testing.T) {
	t.Parallel()
	r := newRig(t, output(100, 10), output(100, 10), output(100, 10))
	r.req.Repeats = 3
	if _, err := r.runner.Measure(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	for i, c := range r.engine.calls {
		if c.slotsHeld != 1 {
			t.Errorf("call %d held %d slots", i, c.slotsHeld)
		}
	}
	if r.slots.acquired != 3 || r.slots.held != 0 {
		t.Fatalf("acquired %d, still held %d", r.slots.acquired, r.slots.held)
	}

	failing := newRig(t)
	failing.engine.reply = func(int) (checks.CallResult, error) { return checks.CallResult{}, errors.New("cannot start dagger") }
	if _, err := failing.runner.Measure(context.Background(), failing.req); err == nil || failing.slots.held != 0 {
		t.Fatalf("an engine that cannot start: %v, %d slots held", err, failing.slots.held)
	}

	refused := newRig(t, output(100, 10))
	refused.slots.refuse = errors.New("no slot")
	if _, err := refused.runner.Measure(context.Background(), refused.req); err == nil || len(refused.engine.calls) != 0 {
		t.Fatalf("a refused slot: %v, %d calls", err, len(refused.engine.calls))
	}
}

func TestRunnerEndsARunAtItsOwnTimeout(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.block = true
	r.runner.Timeout = 30 * time.Millisecond
	m, err := r.runner.Measure(context.Background(), r.req)
	if err != nil {
		t.Fatal(err)
	}
	if m.Valid || len(m.Runs) != 1 || m.Runs[0].Status != RunFailed || !strings.Contains(m.Runs[0].Reason, "did not finish within 30ms") {
		t.Fatalf("%+v", m)
	}
	if r.slots.held != 0 {
		t.Fatalf("%d slots held after the timeout", r.slots.held)
	}
}

func TestRunnerReturnsAnErrorWhenTheCallerCancels(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.block = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if m, err := r.runner.Measure(ctx, r.req); !errors.Is(err, context.Canceled) {
		t.Fatalf("%+v, %v", m, err)
	}
	if r.slots.held != 0 {
		t.Fatalf("%d slots held after the cancellation", r.slots.held)
	}
}

func TestRunnerRecordsEveryKindOfFailedRunWithAReason(t *testing.T) {
	t.Parallel()
	wrongDirection := strings.Replace(output(100, 10), `"unit":"ms","direction":"lower"`, `"unit":"ms","direction":"higher"`, 1)
	wrongUnit := strings.Replace(output(100, 10), `"unit":"ms"`, `"unit":"s"`, 1)
	noPrimary := `{"schema":1,"valid":true,"reason":"ok","metrics":{"peak_rss_mb":{"value":10,"unit":"MB","direction":"lower"}}}`
	noSecondary := `{"schema":1,"valid":true,"reason":"ok","metrics":{"wall_ms":{"value":100,"unit":"ms","direction":"lower"}}}`
	for _, c := range []struct {
		name   string
		result checks.CallResult
		status RunStatus
		want   string
	}{
		{"omits the primary metric", checks.CallResult{Stdout: noPrimary}, RunFailed, `lacks the primary metric "wall_ms"`},
		{"omits a guardrail-limited secondary metric", checks.CallResult{Stdout: noSecondary}, RunFailed, `lacks the secondary metric "peak_rss_mb"`},
		{"reports another direction", checks.CallResult{Stdout: wrongDirection}, RunFailed, `reports direction "higher"`},
		{"reports another unit", checks.CallResult{Stdout: wrongUnit}, RunFailed, `reports unit "s", but calibration recorded "ms"`},
		{"is not JSON", checks.CallResult{Stdout: "done"}, RunFailed, "not a JSON object"},
		{"has the wrong schema", checks.CallResult{Stdout: `{"schema":9,"valid":true,"reason":"","metrics":{}}`}, RunFailed, "unsupported schema 9"},
		{"has no output", checks.CallResult{}, RunFailed, "no output"},
		{"exits non-zero", checks.CallResult{ExitCode: 1, Stderr: "Error: oracle crashed\n"}, RunFailed, "exited 1: Error: oracle crashed"},
		{"outgrows the output limit", checks.CallResult{Stdout: output(1, 1), Truncated: true}, RunFailed, "too large"},
		{"reports valid false", checks.CallResult{Stdout: `{"schema":1,"valid":false,"reason":"3 outputs differ from the oracle","metrics":{"wall_ms":{"value":1,"unit":"ms","direction":"lower"}}}`}, RunInvalid, "3 outputs differ from the oracle"},
		{"reports valid false without a reason", checks.CallResult{Stdout: `{"schema":1,"valid":false,"reason":"","metrics":{}}`}, RunInvalid, "without a reason"},
	} {
		r := newRig(t)
		r.engine.reply = func(int) (checks.CallResult, error) { return c.result, nil }
		m, err := r.runner.Measure(context.Background(), r.req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		run := m.Runs[0]
		if run.Status != c.status || !strings.Contains(run.Reason, c.want) || m.Valid || m.Score != 0 || m.Scores != nil {
			t.Errorf("%s: run %+v, measurement %+v", c.name, run, m)
		}
		if run.Output != c.result.Stdout {
			t.Errorf("%s: the full output was not recorded: %q", c.name, run.Output)
		}
	}
}

func TestARunThatScoredRecordsItsFullOutputAndParsedValues(t *testing.T) {
	t.Parallel()
	r := newRig(t, output(100, 10))
	m, err := r.runner.Measure(context.Background(), r.req)
	if err != nil || !m.Valid {
		t.Fatalf("%+v, %v", m, err)
	}
	run := m.Runs[0]
	if run.Status != RunScored || run.Output != output(100, 10) || run.Parsed == nil || run.Parsed.Metrics["wall_ms"].Value != 100 || run.StartedAt.IsZero() {
		t.Fatalf("%+v", run)
	}
}

func TestOneBadRunMakesAKRunMeasurementInvalidWhateverTheOthersScore(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		"failed run":  `{"schema":1,"valid":true,"reason":"ok","metrics":{}}`,
		"invalid run": `{"schema":1,"valid":false,"reason":"wrong answer","metrics":{}}`,
	} {
		r := newRig(t, output(50, 10), bad, output(50, 10))
		r.req.Repeats = 3
		m, err := r.runner.Measure(context.Background(), r.req)
		if err != nil {
			t.Fatal(err)
		}
		if m.Valid || m.FailedRun != 2 || m.Score != 0 || m.Scores != nil || m.Metrics != nil || len(m.Runs) != 2 || len(r.engine.calls) != 2 {
			t.Fatalf("%s: %+v after %d calls", name, m, len(r.engine.calls))
		}
		d := Decide(Candidate{Measurement: m, Best: 100, Direction: Lower, MinImprovement: 5, ChecksGreen: true})
		if d.Keep || d.Cause != CauseInvalid {
			t.Fatalf("%s: a measurement whose other runs would beat the best was kept: %+v", name, d)
		}

		again := newRig(t, output(50, 10), bad, output(50, 10))
		calibration, err := again.runner.Calibrate(context.Background(), again.req, 3, 5)
		if err != nil {
			t.Fatal(err)
		}
		if calibration.Verdict != BrokenMetric || calibration.Baseline != 0 || calibration.Metrics != nil {
			t.Fatalf("%s: calibration of the same runs: %+v", name, calibration)
		}
	}
}

func TestCalibrationThroughTheRunnerEstablishesBaselineUnitsAndSpread(t *testing.T) {
	t.Parallel()
	r := newRig(t, output(100, 10), output(104, 11), output(102, 12))
	r.req.Expect = expectations()
	c, err := r.runner.Calibrate(context.Background(), r.req, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verdict != Calibrated || c.Baseline != 102 || c.Spread != 4 || c.Units["wall_ms"] != "ms" || c.Units["peak_rss_mb"] != "MB" || c.Metrics["peak_rss_mb"] != 11 {
		t.Fatalf("%+v", c)
	}
	noisy := newRig(t, output(100, 10), output(110, 10), output(105, 10))
	noisy.req.Expect = expectations()
	c, err = noisy.runner.Calibrate(context.Background(), noisy.req, 3, 10)
	if err != nil || c.Verdict != TooNoisy || c.Spread != 10 || c.Baseline != 105 {
		t.Fatalf("%+v, %v", c, err)
	}
}

func TestRunnerRefusesARequestItCannotMeasure(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*rig){
		"no commit":       func(r *rig) { r.req.Commit = "" },
		"option as name":  func(r *rig) { r.req.Function = "--help" },
		"no approved":     func(r *rig) { r.req.Approved = nil },
		"no repeats":      func(r *rig) { r.req.Repeats = 0 },
		"bad expectation": func(r *rig) { r.req.Expect.Primary.Name = "" },
		"no timeout":      func(r *rig) { r.runner.Timeout = 0 },
		"no engine":       func(r *rig) { r.runner.Engine = nil },
	} {
		r := newRig(t, output(1, 1))
		mutate(r)
		if _, err := r.runner.Measure(context.Background(), r.req); err == nil || len(r.engine.calls) != 0 {
			t.Errorf("%s: %v, %d calls", name, err, len(r.engine.calls))
		}
	}
}
