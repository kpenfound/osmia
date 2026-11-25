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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const loopBranch = "osmia/" + string(fixturekit.Stream)

type script struct {
	pending       bool
	noCandidate   string
	changed       []string
	report        AttemptReport
	failChecks    bool
	reject        bool
	reviewNote    string
	pendingReview bool
}

type fakeHost struct {
	mu         sync.Mutex
	t          *testing.T
	branches   map[string]string
	workspaces map[int]string
	scripts    map[int]*script
	seen       map[int]int
	reviewSeen map[int]int
	briefs     map[int]Brief
	threads    []int
	moves      []string
	squashes   []string
	reviews    []int
	guarded    []int
	dropped    []int
}

func newFakeHost(t *testing.T) *fakeHost {
	return &fakeHost{t: t, branches: map[string]string{loopBranch: "base0001"}, workspaces: map[int]string{}, scripts: map[int]*script{}, seen: map[int]int{}, reviewSeen: map[int]int{}, briefs: map[int]Brief{}}
}

func (h *fakeHost) script(n int, s script) { h.scripts[n] = &s }

func (h *fakeHost) Export(_ context.Context, commit, dir string) error {
	for name, content := range map[string]string{"candidate.txt": commit, "dagger.toml": evalToml, "dagger.lock": "lock\n", "fixtures/input.txt": "input\n", ".dagger/eval/main.go": evaluatorSource, "src/work.go": "package src\n"} {
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

func (h *fakeHost) Branch(_ context.Context, name string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	commit, ok := h.branches[name]
	return commit, ok, nil
}

func (h *fakeHost) ChangedPaths(_ context.Context, _, candidate string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for n, s := range h.scripts {
		if candidate == fmt.Sprintf("cand%04d", n) {
			return slices.Clone(s.changed), nil
		}
	}
	return nil, fmt.Errorf("unknown candidate %s", candidate)
}

func (h *fakeHost) Diff(_ context.Context, base, candidate string) (string, error) {
	return fmt.Sprintf("diff --git a/src/work.go b/src/work.go\n--- a/src/work.go\n+++ b/src/work.go\n+// %s on %s\n", candidate, base), nil
}

func (h *fakeHost) Squash(_ context.Context, base, candidate, _ string, at time.Time) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := fmt.Sprintf("sq-%s-%s-%d", base, candidate, at.UnixNano())
	h.squashes = append(h.squashes, id)
	return id, nil
}

func (h *fakeHost) Open(_ context.Context, n int, parent string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	branch := fmt.Sprintf("osmia-unit/%s/attempt-%d", fixturekit.Stream, n)
	if _, exists := h.branches[branch]; !exists {
		h.branches[branch] = parent
	}
	h.workspaces[n] = branch
	return branch, nil
}

func (h *fakeHost) Capture(_ context.Context, n int, parent string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.scripts[n]
	if s == nil || len(s.changed) == 0 {
		return parent, nil
	}
	candidate := fmt.Sprintf("cand%04d", n)
	h.branches[h.workspaces[n]] = candidate
	return candidate, nil
}

func (h *fakeHost) Drop(_ context.Context, n int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.workspaces, n)
	h.dropped = append(h.dropped, n)
	return nil
}

func (h *fakeHost) Advance(_ context.Context, _, from, to string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch h.branches[loopBranch] {
	case to:
		return nil
	case from:
		h.branches[loopBranch] = to
		h.moves = append(h.moves, from+">"+to)
		return nil
	}
	return fmt.Errorf("the climb branch is at %s, not %s", h.branches[loopBranch], from)
}

func (h *fakeHost) Mason(_ context.Context, req MasonRequest) (MasonResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := req.Journal.Number
	h.briefs[n] = req.Brief
	if !slices.Contains(h.threads, n) {
		h.threads = append(h.threads, n)
	}
	s := h.scripts[n]
	if s == nil {
		return MasonResult{NoCandidate: "the fake mason has no script"}, nil
	}
	h.seen[n]++
	if s.pending && h.seen[n] == 1 {
		return MasonResult{Pending: true}, nil
	}
	if s.noCandidate != "" {
		return MasonResult{NoCandidate: s.noCandidate}, nil
	}
	report := s.report
	return MasonResult{Report: &report}, nil
}

func (h *fakeHost) Review(_ context.Context, req ReviewRequest) (ReviewResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := req.Journal.Number
	h.reviewSeen[n]++
	s := h.scripts[n]
	if s.pendingReview && h.reviewSeen[n] == 1 {
		return ReviewResult{Pending: true}, nil
	}
	if !slices.Contains(h.reviews, n) {
		h.reviews = append(h.reviews, n)
	}
	if s.reject {
		return ReviewResult{Review: Review{Approved: false, Summary: "reads like gaming", Notes: s.reviewNote, Turn: "review"}}, nil
	}
	return ReviewResult{Review: Review{Approved: true, Summary: "fine", Turn: "review"}}, nil
}

func (h *fakeHost) Guardrails(_ context.Context, req GuardrailRequest) (Guardrails, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.guarded = append(h.guarded, req.Number)
	if h.scripts[req.Number].failChecks {
		return Guardrails{Green: false, Detail: "go:test failed"}, nil
	}
	return Guardrails{Green: true}, nil
}

type loopEngine struct {
	mu      sync.Mutex
	outputs map[string]string
	calls   []string
	runs    []string
}

func (e *loopEngine) Call(_ context.Context, dir, _, run string) (checks.CallResult, error) {
	data, err := os.ReadFile(filepath.Join(dir, "candidate.txt"))
	if err != nil {
		return checks.CallResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, string(data))
	e.runs = append(e.runs, run)
	out, found := e.outputs[string(data)]
	if !found {
		return checks.CallResult{}, fmt.Errorf("no output scripted for %s", data)
	}
	return ok(out)
}

type loopRig struct {
	t        *testing.T
	repo     *trace.Repository
	host     *fakeHost
	engine   *loopEngine
	slots    *fakeSlots
	spec     Spec
	revision int
	specs    map[int]Spec
	loop     *Loop
	crashAt  string
	crashed  bool
	stops    []StopRequest
	queue    []StopOutcome
	spent    float64
	events   []api.EventKind
	now      time.Time
	allow    string
	approval ApprovalRecord
}

func newLoopRig(t *testing.T, mutate func(*Spec)) *loopRig {
	t.Helper()
	repo := newAttemptStore(t)
	spec := climbSpec()
	if mutate != nil {
		mutate(&spec)
	}
	slots := &fakeSlots{}
	engine := &loopEngine{outputs: map[string]string{}}
	r := &loopRig{t: t, repo: repo, host: newFakeHost(t), engine: engine, slots: slots, spec: spec, revision: 1, specs: map[int]Spec{1: spec}, now: fixturekit.DemoStart.Add(time.Hour)}
	r.approve()
	r.loop = r.newLoop()
	return r
}

func (r *loopRig) approve() {
	r.t.Helper()
	ctx := context.Background()
	protected := []string{".dagger/eval", "dagger.lock", "dagger.toml", "fixtures"}
	dir := r.t.TempDir()
	fixturekit.Must(r.t, r.host.Export(ctx, "base0001", dir))
	approved, err := Snapshot(dir, protected)
	fixturekit.Must(r.t, err)
	r.approval = ApprovalRecord{Evaluator: r.spec.Evaluator, Commit: "base0001", Baseline: 100, Units: map[string]string{"wall_ms": "ms", "peak_rss_mb": "MB"}, Noise: 1, Protected: protected, Approved: approved, ApprovedBy: "owner", At: fixturekit.DemoStart, SpecRevision: 1, Epoch: 1}
	h := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
	}
	_, err = r.repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("fixture-building"), Subject: trace.FeatureSubject, To: "building", Reason: "fixture"}})
	fixturekit.Must(r.t, err)
	m := &Machine{Repository: r.repo, Now: func() time.Time { return fixturekit.DemoStart }}
	doc, err := m.document(fixturekit.Stream, trace.ClimbApprovalID, trace.ClimbApprovalPath, 1, "fixture", r.approval)
	fixturekit.Must(r.t, err)
	_, err = r.repo.RecordDocumentsWith(ctx, []trace.Document{doc}, trace.Transaction{Transition: trace.Transition{Header: h("fixture-climbing"), Subject: InnerSubject, To: InnerClimbing, Reason: "fixture"}})
	fixturekit.Must(r.t, err)
}

func (r *loopRig) newLoop() *Loop {
	return &Loop{
		Repository: r.repo,
		Spec:       func(config.WorkstreamID) (Spec, int, error) { return r.spec, r.revision, nil },
		SpecAt:     func(_ config.WorkstreamID, revision int) (Spec, error) { return r.specs[revision], nil },
		Branch:     func(stream config.WorkstreamID) string { return "osmia/" + string(stream) },
		Host:       func(config.WorkstreamID) (Host, error) { return r.host, nil },
		Runner: func(_ config.WorkstreamID, source Source) (Runner, error) {
			return Runner{Source: source, Engine: r.engine, AcquireSlot: r.slots.acquire, Root: filepath.Join(r.t.TempDir(), "measure"), Timeout: time.Minute}, nil
		},
		Spent: func(config.WorkstreamID, time.Time) (float64, error) { return r.spent, nil },
		Allow: func(context.Context, config.WorkstreamID) (string, error) { return r.allow, nil },
		Stop: func(_ context.Context, req StopRequest) (StopOutcome, error) {
			r.stops = append(r.stops, req)
			outcome := StopWait
			if len(r.queue) > 0 {
				outcome, r.queue = r.queue[0], r.queue[1:]
			}
			if outcome == StopConclude {
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("fixture-conclude-%d", req.After), Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: r.now, Actor: InnerActor, Cause: "fixture"}
				_, err := r.repo.TransactWhileFeature(context.Background(), trace.Transaction{ExpectedVersion: req.State.Version, Transition: trace.Transition{Header: h, Subject: InnerSubject, From: InnerClimbing, To: InnerConcluding, Reason: "fixture"}}, featureBuilding)
				return StopConclude, err
			}
			return outcome, nil
		},
		Publish: func(kind api.EventKind, _ config.WorkstreamID) { r.events = append(r.events, kind) },
		Now:     func() time.Time { return r.now },
		Step: func(name string) error {
			if r.crashAt != "" && name == r.crashAt && !r.crashed {
				r.crashed = true
				return errors.New("crash at " + name)
			}
			return nil
		},
	}
}

func (r *loopRig) reconcile() {
	r.t.Helper()
	fixturekit.Must(r.t, r.loop.Reconcile(context.Background(), fixturekit.Stream))
}

func (r *loopRig) crash(name string) {
	r.t.Helper()
	r.crashAt, r.crashed = name, false
	err := r.loop.Reconcile(context.Background(), fixturekit.Stream)
	if err == nil || !strings.Contains(err.Error(), "crash at "+name) {
		r.t.Fatalf("pass ended with %v, want a crash at %s", err, name)
	}
	r.crashAt = ""
	r.loop = r.newLoop()
}

func (r *loopRig) attempts() []Attempt {
	r.t.Helper()
	got, err := ReadAttempts(r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	return got
}

func (r *loopRig) attempt(n int) Attempt {
	r.t.Helper()
	a, found, err := ReadAttempt(r.repo, fixturekit.Stream, n)
	fixturekit.Must(r.t, err)
	if !found {
		r.t.Fatalf("attempt %d is not recorded; recorded %d", n, len(r.attempts()))
	}
	return a
}

func (r *loopRig) phase(n int) string {
	state, err := r.repo.Workflow(fixturekit.Stream, trace.ClimbAttemptSubject(n))
	fixturekit.Must(r.t, err)
	return state.Value
}

func (r *loopRig) tip() string { return r.host.branches[loopBranch] }

func edits(paths ...string) []string { return paths }

func good(h string) AttemptReport {
	return AttemptReport{Hypothesis: h, Change: "changes src/work.go", Expected: "less wall time"}
}

func (r *loopRig) score(n int, wall, rss float64) {
	r.engine.outputs[fmt.Sprintf("cand%04d", n)] = output(wall, rss)
}

func (r *loopRig) scoreRaw(n int, out string) { r.engine.outputs[fmt.Sprintf("cand%04d", n)] = out }

func TestAttemptsRunOneAtATimeFromTheCurrentBestWithTheLogSteeringAndRejectionNotes(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 2 })
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
	r.host.script(2, script{changed: edits("src/work.go"), report: good("skip validation"), reject: true, reviewNote: "this skips the oracle, which games the metric"})
	r.host.script(3, script{changed: edits("src/work.go"), report: good("batch the reads")})
	r.score(1, 90, 120)
	r.score(2, 80, 120)
	r.score(3, 70, 120)

	r.reconcile()
	if len(r.attempts()) != 2 {
		t.Fatalf("recorded attempts %d, want 2", len(r.attempts()))
	}
	a1 := r.attempt(1)
	if a1.Outcome != OutcomeKept || a1.Parent != "base0001" || a1.Best != 100 || a1.Commit == "" || r.tip() != a1.Commit {
		t.Fatalf("attempt 1 %+v, climb branch %s", a1, r.tip())
	}
	if a2 := r.attempt(2); a2.Outcome != OutcomeDiscarded || a2.Cause != CauseRejected || a2.Parent != a1.Commit || a2.Best != 90 || a2.ReviewNotes == "" || a2.Branch == "" {
		t.Fatalf("attempt 2 %+v", a2)
	}
	if got := r.tip(); got != a1.Commit {
		t.Fatalf("a rejected attempt moved the climb branch to %s", got)
	}
	if _, err := Steer(context.Background(), r.repo, fixturekit.Stream, "try batching the reads", "chief", r.now); err != nil {
		t.Fatal(err)
	}
	extension, _, err := BudgetDocument(r.repo, fixturekit.Stream, Extension{Attempts: 1}, r.now)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.repo.RecordDocuments(context.Background(), []trace.Document{extension}))
	r.reconcile()
	if len(r.attempts()) != 3 {
		t.Fatalf("recorded attempts %d, want 3", len(r.attempts()))
	}
	if !slices.Equal(r.host.threads, []int{1, 2, 3}) {
		t.Fatalf("the mason was asked about attempts %v, one at a time and in order", r.host.threads)
	}
	b2, b3 := r.host.briefs[2], r.host.briefs[3]
	if b2.Parent != a1.Commit || len(b2.Log) != 1 || b2.Log[0].Number != 1 || b2.Rejection != nil || b2.Best != 90 {
		t.Fatalf("brief of attempt 2: %+v", b2)
	}
	text := b3.Render()
	for _, want := range []string{"The reviewer rejected attempt 2", "this skips the oracle, which games the metric", "try batching the reads (new)", "- attempt 1: kept", "- attempt 2: discarded (review-rejected)", "cache the parse", "skip validation", a1.Commit} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief of attempt 3 lacks %q:\n%s", want, text)
		}
	}
	if b3.Parent != a1.Commit {
		t.Errorf("attempt 3 started from %s, not the best %s", b3.Parent, a1.Commit)
	}
	if a3 := r.attempt(3); a3.Outcome != OutcomeKept || a3.Parent != a1.Commit || r.tip() != a3.Commit {
		t.Fatalf("attempt 3 %+v", a3)
	}
	if len(r.host.workspaces) != 0 {
		t.Fatalf("workspaces left: %v", r.host.workspaces)
	}
	for _, n := range []int{1, 2, 3} {
		if r.phase(n) != string(PhaseCleaned) {
			t.Errorf("attempt %d is %q", n, r.phase(n))
		}
	}
}

func TestAnAttemptTouchingAProtectedPathIsRefusedBeforeMeasurement(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"dagger.toml", "dagger.lock", ".dagger/eval/main.go", "fixtures/input.txt"} {
		r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 1 })
		r.host.script(1, script{changed: edits("src/work.go", path), report: good("tune the evaluator")})
		r.reconcile()
		a := r.attempt(1)
		if a.Outcome != OutcomeDiscarded || a.Cause != CauseProtected || !strings.Contains(a.Reason, path) || a.Measurement != nil {
			t.Fatalf("an attempt touching %s: %+v", path, a)
		}
		if len(r.engine.calls) != 0 || len(r.host.reviews) != 0 || len(r.host.guarded) != 0 {
			t.Fatalf("%s: measured %d times, reviewed %v, checked %v", path, len(r.engine.calls), r.host.reviews, r.host.guarded)
		}
		if r.tip() != "base0001" || a.Branch == "" || r.host.branches[a.Branch] != "cand0001" {
			t.Fatalf("%s: the climb branch is %s and the attempt branch %s holds %s", path, r.tip(), a.Branch, r.host.branches[a.Branch])
		}
	}
}

func TestOnlyAnAttemptMeetingTheKeepConditionsReachesTheReviewer(t *testing.T) {
	t.Parallel()
	invalid := `{"schema":1,"valid":false,"reason":"the output differs from the oracle","metrics":{}}`
	cases := []struct {
		name   string
		script script
		output string
		cause  Cause
		review bool
		engine int
	}{
		{"a failing guardrail check", script{failChecks: true}, "", CauseGuardrails, false, 0},
		{"a secondary limit", script{}, output(80, 250), CauseLimits, false, 1},
		{"an invalid measurement", script{}, invalid, CauseInvalid, false, 1},
		{"an improvement short of the minimum", script{}, output(96, 120), CauseNoImprovement, false, 1},
		{"a regression", script{}, output(120, 120), CauseNoImprovement, false, 1},
		{"a keep", script{}, output(95, 120), "", true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t, func(s *Spec) {
				s.Stop.MaxAttempts = 1
				s.Guardrails.Checks = []string{"go:test"}
			})
			tc.script.changed, tc.script.report = edits("src/work.go"), good("h")
			r.host.script(1, tc.script)
			r.scoreRaw(1, tc.output)
			r.reconcile()
			a := r.attempt(1)
			if got := len(r.host.reviews) == 1; got != tc.review {
				t.Fatalf("reviewed: %v, want %v", got, tc.review)
			}
			if got := len(r.engine.calls); got != tc.engine {
				t.Fatalf("the evaluator ran %d times, want %d", got, tc.engine)
			}
			switch {
			case tc.review:
				if a.Outcome != OutcomeKept || a.Scores == nil || a.Scores.Primary != 95 || a.Change == nil || *a.Change != -5 {
					t.Fatalf("kept attempt %+v", a)
				}
			default:
				if a.Outcome != OutcomeDiscarded || a.Cause != tc.cause || a.Reason == "" {
					t.Fatalf("attempt %+v, want a discard for %s", a, tc.cause)
				}
				if r.tip() != "base0001" {
					t.Fatalf("a discarded attempt moved the climb branch to %s", r.tip())
				}
			}
		})
	}
}

func TestAnApprovedAttemptLandsAsOneSquashedCommitAndMovesTheBest(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 1 })
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
	r.score(1, 90, 120)
	r.reconcile()
	a := r.attempt(1)
	if len(r.host.moves) != 1 || r.host.moves[0] != "base0001>"+a.Commit || r.tip() != a.Commit {
		t.Fatalf("moves %v, tip %s", r.host.moves, r.tip())
	}
	j, found, err := ReadJournal(r.repo, fixturekit.Stream, 1)
	fixturekit.Must(t, err)
	if !found || j.Squashed != a.Commit || !strings.Contains(j.Message, "cache the parse") || j.Phase != PhaseCleaned {
		t.Fatalf("journal %+v", j)
	}
	notices := outbox(t, r.repo, trace.ProgressKind)
	if !slices.ContainsFunc(notices, func(body string) bool { return strings.Contains(body, "Attempt 1 was kept") }) {
		t.Fatalf("the chief was not told: %v", notices)
	}
	running, found, err := ReadRunning(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !found || running.Phase != "" || running.Attempt != 0 {
		t.Fatalf("running after the attempt: %+v", running)
	}
	for _, kind := range []api.EventKind{api.EventClimbAttemptStarted, api.EventClimbPhase, api.EventClimbAttemptRecorded, api.EventClimbKept} {
		if !slices.Contains(r.events, kind) {
			t.Errorf("no %s event in %v", kind, r.events)
		}
	}
}

func outbox(t *testing.T, repo *trace.Repository, kind string) []string {
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

func TestRunningTracksThePhasesOfTheAttempt(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Repeats = 2; s.Stop.MaxAttempts = 1 })
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse"), pendingReview: true})
	r.score(1, 90, 120)
	r.reconcile()
	running, _, err := ReadRunning(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if running.Attempt != 1 || running.Phase != kinds.PhaseReviewing || running.Hypothesis != "cache the parse" || running.ClimbStartedAt.IsZero() {
		t.Fatalf("running while the reviewer reads: %+v", running)
	}
	docs, err := trace.Read[trace.Document](r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	var phases []string
	for _, d := range docs {
		if d.ID == trace.ClimbRunningID {
			var run Running
			fixturekit.Must(t, jsonUnmarshal(d.Content, &run))
			phases = append(phases, fmt.Sprintf("%s %d/%d", run.Phase, run.Run, run.Runs))
		}
	}
	for _, want := range []string{"mason 0/0", "guardrails 0/0", "measuring 1/2", "measuring 2/2", "reviewing 0/0"} {
		if !slices.Contains(phases, want) {
			t.Errorf("phases %v lack %q", phases, want)
		}
	}
	r.reconcile()
	if running, _, _ := ReadRunning(r.repo, fixturekit.Stream); running.Phase != "" {
		t.Fatalf("running after the terminal record: %+v", running)
	}
}

func TestARestartAtAnyPointEndsTheAttemptWithOneRecordOneChargeAndTheRightBest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		hook string
		keep bool
	}{
		{"climb:started", true},
		{"climb:captured-unrecorded", true},
		{"climb:captured", true},
		{"climb:measuring", true},
		{"climb:measured", true},
		{"climb:reviewed", true},
		{"climb:promoting", true},
		{"climb:moving", true},
		{"climb:moved", true},
		{"climb:promoted", true},
		{"climb:recorded", true},
		{"climb:dropping", true},
		{"climb:cleaned", true},
		{"climb:measuring", false},
		{"climb:recorded", false},
		{"climb:dropping", false},
	} {
		t.Run(fmt.Sprintf("%s keep=%v", tc.hook, tc.keep), func(t *testing.T) {
			r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 2 })
			wall := 90.0
			if !tc.keep {
				wall = 99
			}
			r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
			r.host.script(2, script{changed: edits("src/work.go"), report: good("second idea")})
			r.score(1, wall, 120)
			r.score(2, 80, 120)
			r.crash(tc.hook)
			r.reconcile()

			attempts := r.attempts()
			if len(attempts) != 2 || attempts[0].Number != 1 || attempts[1].Number != 2 {
				t.Fatalf("recorded attempts %+v", attempts)
			}
			a := attempts[0]
			if tc.keep != (a.Outcome == OutcomeKept) {
				t.Fatalf("attempt 1 %+v", a)
			}
			var lastKept string
			for _, x := range attempts {
				if x.Outcome == OutcomeKept {
					lastKept = x.Commit
				}
			}
			if r.tip() != lastKept && !(lastKept == "" && r.tip() == "base0001") {
				t.Fatalf("the climb branch is at %s, the last kept commit is %q", r.tip(), lastKept)
			}
			if tc.keep && (len(r.host.moves) != 2 || attempts[1].Parent != a.Commit) {
				t.Fatalf("moves %v, attempt 2 parent %s", r.host.moves, attempts[1].Parent)
			}
			if got := attempts[1].Parent; !tc.keep && got != "base0001" {
				t.Fatalf("attempt 2 started from %s after a discard", got)
			}
			if !tc.keep && len(r.host.moves) != 1 {
				t.Fatalf("moves %v", r.host.moves)
			}
			if a.Branch == "" || r.host.branches[a.Branch] != "cand0001" {
				t.Fatalf("the branch of attempt 1 is %q holding %q", a.Branch, r.host.branches[a.Branch])
			}
			if len(r.host.workspaces) != 0 {
				t.Fatalf("live workspaces %v", r.host.workspaces)
			}
			docs, err := trace.Read[trace.Document](r.repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			records := 0
			for _, d := range docs {
				if d.Path == trace.ClimbAttemptPath(1) {
					records++
				}
			}
			if records != 1 {
				t.Fatalf("attempt 1 has %d terminal records", records)
			}
			if got := len(outbox(t, r.repo, trace.ProgressKind)); tc.keep && got != 2 || !tc.keep && got != 1 {
				t.Fatalf("%d kept-attempt events to the chief", got)
			}
		})
	}
}

func TestThreeAttemptsInARowWithoutACandidateRaiseOneStallToTheChief(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 6 })
	for n := 1; n <= 3; n++ {
		r.host.script(n, script{noCandidate: "the mason ended 3 clean turns without calling done"})
	}
	r.host.script(4, script{changed: edits("src/work.go"), report: good("h")})
	r.score(4, 99, 120)
	r.reconcile()
	attempts := r.attempts()
	if len(attempts) < 4 {
		t.Fatalf("attempts %d", len(attempts))
	}
	for n := 1; n <= 3; n++ {
		if a := attempts[n-1]; a.Cause != CauseNoCandidate || a.Outcome != OutcomeDiscarded {
			t.Fatalf("attempt %d: %+v", n, a)
		}
	}
	stalls := outbox(t, r.repo, trace.NoticeKind)
	if len(stalls) != 1 || !strings.Contains(stalls[0], "3 attempts in a row ended without a candidate") {
		t.Fatalf("notices to the chief: %v", stalls)
	}
	if attempts[3].Cause == CauseNoCandidate {
		t.Fatalf("attempt 4 %+v", attempts[3])
	}
}

func TestAnEmptyWorkspaceIsNoCandidate(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 1 })
	r.host.script(1, script{report: good("nothing worth trying")})
	r.reconcile()
	if a := r.attempt(1); a.Cause != CauseNoCandidate || !strings.Contains(a.Reason, "no change") || len(r.engine.calls) != 0 {
		t.Fatalf("attempt %+v", a)
	}
}

func TestTheLoopWaitsWhileTheMasonOrTheReviewerIsStillWorking(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 1 })
	r.host.script(1, script{changed: edits("src/work.go"), report: good("h"), pending: true, pendingReview: true})
	r.score(1, 90, 120)
	r.reconcile()
	if r.phase(1) != string(PhaseStarted) || len(r.attempts()) != 0 {
		t.Fatalf("phase %q, attempts %d", r.phase(1), len(r.attempts()))
	}
	r.reconcile()
	if r.phase(1) != string(PhaseMeasured) || len(r.host.reviews) != 0 {
		t.Fatalf("phase %q reviews %v", r.phase(1), r.host.reviews)
	}
	r.reconcile()
	if r.attempt(1).Outcome != OutcomeKept || len(r.engine.calls) != 1 {
		t.Fatalf("attempt %+v after %d evaluator calls", r.attempt(1), len(r.engine.calls))
	}
}

func TestNoAttemptStartsWhileWorkIsHeldBack(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 1 })
	r.allow = "the daily budget is spent"
	r.reconcile()
	if n, _ := LastStarted(r.repo, fixturekit.Stream); n != 0 {
		t.Fatalf("attempt %d started", n)
	}
	r.allow = ""
	r.host.script(1, script{noCandidate: "x"})
	r.reconcile()
	if n, _ := LastStarted(r.repo, fixturekit.Stream); n == 0 {
		t.Fatal("no attempt started once work was allowed")
	}
}

func TestAnAttemptBudgetRaisesAHardStopAndAPlateauASoftOne(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 2 })
	for n := 1; n <= 3; n++ {
		r.host.script(n, script{changed: edits("src/work.go"), report: good("idea")})
		r.score(n, 90-float64(n)*10, 120)
	}
	r.reconcile()
	if len(r.attempts()) != 2 || len(r.stops) != 1 {
		t.Fatalf("attempts %d, stops %d", len(r.attempts()), len(r.stops))
	}
	stop := r.stops[0]
	if stop.After != 2 || len(stop.Firings) != 1 || stop.Firings[0].Rule != RuleAttempts || !stop.Firings[0].Hard || stop.State.Value != InnerClimbing {
		t.Fatalf("stop %+v", stop)
	}
	r.reconcile()
	if n, _ := LastStarted(r.repo, fixturekit.Stream); n != 2 || len(r.stops) != 2 {
		t.Fatalf("a climb waiting at a stop started attempt %d (stops raised %d)", n, len(r.stops))
	}
	doc, _, err := BudgetDocument(r.repo, fixturekit.Stream, Extension{Attempts: 1}, r.now)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	r.reconcile()
	if len(r.attempts()) != 3 || len(r.stops) != 3 || r.stops[2].After != 3 {
		t.Fatalf("after the owner's extension: attempts %d, stops %d", len(r.attempts()), len(r.stops))
	}

	soft := newLoopRig(t, func(s *Spec) { s.Stop.Plateau = 1; s.Stop.MaxAttempts = 9 })
	for n := 1; n <= 2; n++ {
		soft.host.script(n, script{changed: edits("src/work.go"), report: good("idea")})
		soft.score(n, 99, 120)
	}
	soft.queue = []StopOutcome{StopWait}
	soft.reconcile()
	if len(soft.stops) != 1 || soft.stops[0].Firings[0].Rule != RulePlateau || soft.stops[0].Firings[0].Hard || len(soft.attempts()) != 1 {
		t.Fatalf("the plateau stop %+v after %d attempts", soft.stops, len(soft.attempts()))
	}
	soft.queue = []StopOutcome{StopContinue, StopWait}
	soft.reconcile()
	if len(soft.attempts()) != 2 || len(soft.stops) != 3 || soft.stops[2].After != 2 {
		t.Fatalf("after a plateau was continued: attempts %d, stops %d", len(soft.attempts()), len(soft.stops))
	}
}

func TestStopRulesAreEvaluatedFromTheRecords(t *testing.T) {
	t.Parallel()
	kept := func(n int, score float64) Attempt {
		return Attempt{Number: n, Outcome: OutcomeKept, Scores: &Scores{Primary: score}}
	}
	discarded := func(n int) Attempt { return Attempt{Number: n, Outcome: OutcomeDiscarded, Cause: CauseNoImprovement} }
	target := 80.0
	base := StopInput{Rules: StopRules{MaxAttempts: 4, MaxCostUSD: 10, MaxWallClock: "2h", Plateau: 3, Target: &target}, Direction: Lower, Baseline: 100, Started: fixturekit.DemoStart, Now: fixturekit.DemoStart.Add(time.Hour)}
	with := func(f func(*StopInput)) StopInput { in := base; f(&in); return in }
	for name, tc := range map[string]struct {
		in   StopInput
		want []string
	}{
		"no attempt":       {with(func(in *StopInput) {}), nil},
		"within every one": {with(func(in *StopInput) { in.Attempts = []Attempt{kept(1, 95), discarded(2)}; in.Spent = 3 }), nil},
		"the attempt budget": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95), kept(2, 94), kept(3, 93), kept(4, 92)}
		}), []string{RuleAttempts}},
		"extended attempts": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95), kept(2, 94), kept(3, 93), kept(4, 92)}
			in.Extension = Totals{Attempts: 2}
		}), nil},
		"the cost budget": {with(func(in *StopInput) { in.Attempts = []Attempt{kept(1, 95)}; in.Spent = 10 }), []string{RuleCost}},
		"extended cost": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95)}
			in.Spent = 10
			in.Extension = Totals{CostUSD: 5}
		}), nil},
		"the wall clock": {with(func(in *StopInput) { in.Attempts = []Attempt{kept(1, 95)}; in.Now = in.Started.Add(2 * time.Hour) }), []string{RuleWallClock}},
		"extended wall clock": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95)}
			in.Now = in.Started.Add(2 * time.Hour)
			in.Extension = Totals{WallClock: time.Hour}
		}), nil},
		"a plateau": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95), discarded(2), discarded(3), discarded(4)}
			in.Rules.MaxAttempts = 9
		}), []string{RulePlateau}},
		"a plateau continued": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95), discarded(2), discarded(3), discarded(4), discarded(5)}
			in.Rules.MaxAttempts = 9
			in.Continued = []StopRecord{{Attempt: 4, Rules: []string{RulePlateau}}}
		}), nil},
		"a plateau continued and reached again": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 95), discarded(2), discarded(3), discarded(4), discarded(5), discarded(6), discarded(7)}
			in.Rules.MaxAttempts = 9
			in.Continued = []StopRecord{{Attempt: 4, Rules: []string{RulePlateau}}}
		}), []string{RulePlateau}},
		"the target": {with(func(in *StopInput) { in.Attempts = []Attempt{kept(1, 79)} }), []string{RuleTarget}},
		"the target continued": {with(func(in *StopInput) {
			in.Attempts = []Attempt{kept(1, 79)}
			in.Continued = []StopRecord{{Attempt: 1, Rules: []string{RuleTarget}}}
		}), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Rules(EvaluateStops(tc.in)); !slices.Equal(got, tc.want) {
				t.Fatalf("firing %v, want %v", got, tc.want)
			}
		})
	}
	higher := base
	higher.Direction, higher.Baseline = Higher, 100
	target = 120
	higher.Attempts = []Attempt{kept(1, 121)}
	if got := Rules(EvaluateStops(higher)); !slices.Equal(got, []string{RuleTarget}) {
		t.Fatalf("a higher-is-better target: %v", got)
	}
	if !Hard(EvaluateStops(with(func(in *StopInput) { in.Attempts = []Attempt{kept(1, 95)}; in.Spent = 11 }))) {
		t.Fatal("the cost budget is a hard stop")
	}
}

func TestAClimbBeingConcludedDiscardsTheAttemptInFlight(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, nil)
	r.host.script(1, script{pending: true, changed: edits("src/work.go"), report: good("h")})
	r.reconcile()
	if r.phase(1) != string(PhaseStarted) {
		t.Fatalf("phase %q", r.phase(1))
	}
	state, err := ReadInner(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-conclude", Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: r.now, Actor: fixturekit.OwnerActor, Cause: "owner"}
	_, err = r.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: InnerSubject, From: InnerClimbing, To: InnerConcluding, Reason: "conclude now"}})
	fixturekit.Must(t, err)
	r.reconcile()
	if a := r.attempt(1); a.Cause != CauseConcluded || a.Outcome != OutcomeDiscarded {
		t.Fatalf("attempt %+v", a)
	}
	r.reconcile()
	if n, _ := LastStarted(r.repo, fixturekit.Stream); n != 1 || len(r.host.workspaces) != 0 || r.phase(1) != string(PhaseCleaned) {
		t.Fatalf("started %d, workspaces %v, phase %q", n, r.host.workspaces, r.phase(1))
	}
}

func TestSteeringNotesAndBudgetExtensionsAreRecorded(t *testing.T) {
	t.Parallel()
	repo := newAttemptStore(t)
	ctx := context.Background()
	if _, err := Steer(ctx, repo, fixturekit.Stream, "  ", "chief", fixturekit.DemoStart); err == nil {
		t.Fatal("an empty note was accepted")
	}
	for i, text := range []string{"avoid the cache", "try batching"} {
		n, err := Steer(ctx, repo, fixturekit.Stream, text, "chief", fixturekit.DemoStart)
		fixturekit.Must(t, err)
		if n.Number != i+1 {
			t.Fatalf("note %+v", n)
		}
	}
	s, err := ReadSteering(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(s.Notes) != 2 || s.Notes[1].Text != "try batching" {
		t.Fatalf("steering %+v", s)
	}
	if problems := (Extension{}).Problems(); len(problems) == 0 {
		t.Fatal("an empty extension was accepted")
	}
	if problems := (Extension{Attempts: 5, WallClock: "1h"}).Problems(); len(problems) != 0 {
		t.Fatalf("problems %v", problems)
	}
	for _, e := range []Extension{{Attempts: 5}, {CostUSD: 2.5, WallClock: "30m"}} {
		doc, _, err := BudgetDocument(repo, fixturekit.Stream, e, fixturekit.DemoStart)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	}
	b, err := ReadBudget(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if total := b.Total(); len(b.Extensions) != 2 || total.Attempts != 5 || total.CostUSD != 2.5 || total.WallClock != 30*time.Minute || b.Extensions[1].Number != 2 {
		t.Fatalf("budget %+v total %+v", b, total)
	}
}
