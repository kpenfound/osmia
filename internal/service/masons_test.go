package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// masonRoles runs the chief of staff and the mason in a container, the only
// sandbox the fake engine's boundary check accepts.
const masonRoles = chiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

// masonWrote is the file every fake mason writes into its view.
const masonWrote = "internal/trace/built.go"

// fakeMasons plays every mason turn of the fixture. Each turn checks that it
// holds its view's file tools and done alone, records what it saw, writes
// masonWrote into its view and then plays what play holds for the turn,
// which ends the turn failed by returning errFailTurn.
type fakeMasons struct {
	mu       sync.Mutex
	runs     map[string][]agent.Request
	problems []string
	// play holds, by turn ID, what the turn does after writing masonWrote.
	play map[string]func(context.Context, agent.Request, *mcp.ClientSession) error
}

func (m *fakeMasons) turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if slices.Sort(names); !slices.Equal(names, []string{doneTool, "file_read", "file_write"}) {
		m.problems = append(m.problems, fmt.Sprintf("mason tools %v", names))
	}
	view := req.Workspace.Directory()
	if err := os.WriteFile(filepath.Join(view, masonWrote), []byte("package trace\n"), 0644); err != nil {
		m.problems = append(m.problems, err.Error())
	}
	stream := sessionStream(req)
	m.runs[stream] = append(m.runs[stream], req)
	if play := m.play[req.Name]; play != nil {
		if err := play(ctx, req, tools); errors.Is(err, errFailTurn) {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Crashed", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, ErrorSubtype: "execution"}, nil
		} else if err != nil {
			m.problems = append(m.problems, fmt.Sprintf("%s: %v", req.Name, err))
		}
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Built", SessionDir: req.SessionDir, NumTurns: 2}, nil
}

// sessionStream returns the workstream of a thread turn, from its session
// directory threads/<project>/<workstream>/...
func sessionStream(req agent.Request) string {
	parts := strings.Split(filepath.ToSlash(req.SessionDir), "/")
	if i := slices.Index(parts, "threads"); i >= 0 && i+2 < len(parts) {
		return parts[i+2]
	}
	return ""
}

// errFailTurn is what a fake mason's play returns to end its turn failed.
var errFailTurn = errors.New("the turn fails")

// requests returns the mason turns that ran in the workstream.
func (m *fakeMasons) requests(stream config.WorkstreamID) []agent.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.runs[string(stream)])
}

func (m *fakeMasons) check(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.problems) != 0 {
		t.Fatal(strings.Join(m.problems, "\n"))
	}
}

// newMasonFixture is a debate fixture whose service runs thread turns
// through the production reconciler with capacity.masons set to masons, and
// whose architect drafts plan, and whose clone has its upstream. Every
// mason turn is played by the returned fake masons.
func newMasonFixture(t *testing.T, masons int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, masonRoles, func(opts *Options) {
		opts.Threads = Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts}).Threads
		path := filepath.Join(opts.Config.Root, "config.toml")
		data, err := os.ReadFile(path)
		must(t, err)
		must(t, os.WriteFile(path, []byte(strings.Replace(string(data), "committee = 1\n", fmt.Sprintf("committee = 1\nmasons = %d\n", masons), 1)), 0600))
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: drafted}, nil)
	f.upstream(t)
	fake := &fakeMasons{runs: map[string][]agent.Request{}, play: map[string]func(context.Context, agent.Request, *mcp.ClientSession) error{}}
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns["*"] = chief.turn
	f.engine.turns[masonTurnID("resume")] = fake.turn
	f.engine.turns[masonTurnID("dedupe")] = fake.turn
	return f, fake
}

// masonTransitions returns the transitions of the mason controller in the
// workstream's events.jsonl.
func masonTransitions(t *testing.T, f *shedFixture, stream config.WorkstreamID) []transitionMove {
	t.Helper()
	var out []transitionMove
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.Actor == masonActor {
			out = append(out, transitionMove{tr.ID, tr.Subject, tr.From, tr.To, tr.Cause, tr.Reason})
		}
	}
	return out
}

// awaitMasonRan waits until the unit's first mason turn completed.
func (f *shedFixture) awaitMasonRan(t *testing.T, stream config.WorkstreamID, unit string) trace.Thread {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().Thread(stream, masonAgent(unit))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && len(th.Turns) > 0 && !th.Turns[0].CompletedAt.IsZero() {
			if status := th.Turns[0].Status(); status != "idle" {
				t.Fatalf("the mason of unit %s of %s ended %s: %+v", unit, stream, status, th.Turns[0])
			}
			return th
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mason of unit %s of %s never ran: %+v", unit, stream, th)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitMasonTransition waits until the mason controller records a
// transition in the workstream.
func (f *shedFixture) awaitMasonTransition(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for len(masonTransitions(t, f, stream)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the mason controller of %s never recorded a transition", stream)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startedReason is the reason a unit of the fixture's plan is started with.
func (f *shedFixture) startedReason(t *testing.T, stream config.WorkstreamID, unit string) string {
	t.Helper()
	base := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", featureBranch(stream)))
	return fmt.Sprintf("unit %s is the next ready unit of the plan of seal 1; its mason works in the unit's workspace on %s, created from %s at %s", unit, unitBranch(stream, unit), featureBranch(stream), base)
}

func started(unit, reason string) transitionMove {
	return transitionMove{masonTransitionID(unit), trace.UnitSubject(unit), UnitReady, UnitImplementing, trace.UnitSubject(unit) + "-" + UnitReady, reason}
}

// settle lets the service's loop run several more passes.
func settle() { time.Sleep(1500 * time.Millisecond) }

// A building workstream's first ready unit moves to implementing, recorded
// in events.jsonl with the mason controller as its actor, and its mason's
// first turn runs once, on a view of the unit's own workspace created from
// the feature branch, with the unit's bundle in its prompt. What the mason
// wrote is in the workspace afterwards. The workstream's other ready unit
// waits: one unit at a time per workstream, however many mason slots are
// free.
func TestMasonStartsOneReadyUnitPerWorkstream(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, independentPlan)
	defer f.stop(t)
	stream, _ := f.builtAs(t, "design")
	f.awaitMasonRan(t, stream, "resume")
	settle()
	masons.check(t)

	if got, want := masonTransitions(t, f, stream), []transitionMove{started("resume", f.startedReason(t, stream, "resume"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitReady}})

	th, err := f.repository().Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(th.Turns) != 1 || th.Identity.Role != masonRole || th.Identity.Unit != "resume" {
		t.Fatalf("mason thread %+v", th)
	}
	req := th.Turns[0].Request
	if req.TurnID != masonTurnID("resume") || req.Unit != "resume" || req.Cause != masonTransitionID("resume") || req.Actor != masonActor {
		t.Fatalf("mason turn request %+v", req)
	}
	runs := masons.requests(stream)
	if len(runs) != 1 {
		t.Fatalf("mason turns run %d times", len(runs))
	}
	run := runs[0]
	for _, want := range []string{"Build unit resume of this workstream.", "# Unit resume\n", "title: Resume from the last chunk\n", "seal: 1\n", "- spec#1: ", "  proof: new-test TestResume\n", "## Spec\n"} {
		if !strings.Contains(run.Prompt, want) {
			t.Fatalf("mason prompt lacks %q:\n%s", want, run.Prompt)
		}
	}
	if strings.Contains(run.Prompt, "# Unit dedupe") {
		t.Fatalf("mason prompt holds another unit:\n%s", run.Prompt)
	}
	if !strings.Contains(run.SystemPrompt, "You are a mason of the") {
		t.Fatalf("mason system prompt %q", run.SystemPrompt)
	}

	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
	if run.Workspace.Directory() == workspace {
		t.Fatal("the mason turn ran in the workspace itself, not a view of it")
	}
	if data, err := os.ReadFile(filepath.Join(workspace, masonWrote)); err != nil || string(data) != "package trace\n" {
		t.Fatalf("what the mason wrote is not in the unit's workspace: %q %v", data, err)
	}
	if branch := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", workspace, "branch", "--show-current")); branch != unitBranch(stream, "resume") {
		t.Fatalf("the unit's workspace is on %s", branch)
	}
	if _, err := os.Stat(filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "dedupe")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the waiting unit has a workspace: %v", err)
	}
}

// With one mason slot, the slot goes to the workstream first in the
// project's priority order, and the other waits. Nothing starts while the
// factory is paused, and a paused workstream's implementing unit gives its
// slot to the next workstream.
func TestMasonSlotsFollowPriorityAndPause(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	first, _ := f.builtAs(t, "first")
	second, _ := f.builtAs(t, "second")
	settle()
	for _, stream := range []config.WorkstreamID{first, second} {
		if got := masonTransitions(t, f, stream); len(got) != 0 {
			t.Fatalf("%s started a unit while the factory was paused: %+v", stream, got)
		}
	}
	// The runtime store learns of workstreams handed in since it opened on
	// the next start.
	f.stop(t)
	f.start(t)

	// The priority order goes against the workstream ID order, which would
	// otherwise break the tie.
	hi, lo := first, second
	if hi < lo {
		hi, lo = lo, hi
	}
	mutation(t, f.c, "PUT", "priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{hi, lo}})
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, hi, "resume")
	settle()
	if got := masonTransitions(t, f, lo); len(got) != 0 {
		t.Fatalf("%s started a unit with no mason slot free: %+v", lo, got)
	}
	f.checkUnits(t, lo, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitPlanned}})

	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: hi}, Mode: "soft", Source: "operator"})
	f.awaitMasonRan(t, lo, "resume")
	masons.check(t)
	if got, want := masonTransitions(t, f, lo), []transitionMove{started("resume", f.startedReason(t, lo, "resume"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitPlanned}})
}

// A unit moved to implementing whose mason turn was never queued, as after
// a stop between the two, gets its workspace and its turn on the next
// lifetime, once.
func TestImplementingUnitGetsItsMasonTurnAfterARestart(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, validPlan)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	stream, _ := f.builtAs(t, "design")
	f.stop(t)

	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	states, err := repo.WorkflowStates(stream)
	must(t, err)
	subject := trace.UnitSubject("resume")
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: masonTransitionID("resume"), Revision: 1, Project: f.project, Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: masonActor, Cause: subject + "-" + UnitReady}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: states[subject].Version, Transition: trace.Transition{Header: h, Subject: subject, From: UnitReady, To: UnitImplementing, Reason: "planted"}})
	must(t, errors.Join(err, repo.Close()))

	f.start(t)
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, stream, "resume")
	settle()
	masons.check(t)
	if runs := masons.requests(stream); len(runs) != 1 || !strings.Contains(runs[0].Prompt, "# Unit resume\n") {
		t.Fatalf("mason turns %+v", runs)
	}
	if got := masonTransitions(t, f, stream); len(got) != 1 || got[0].Reason != "planted" {
		t.Fatalf("mason transitions %+v", got)
	}
	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
	if _, err := os.Stat(filepath.Join(workspace, masonWrote)); err != nil {
		t.Fatal(err)
	}
}

// staleSpec is validSpec as the owner edits it after the seal.
const staleSpec = validSpec + "9. Something new.\n"

// editSpec writes the workstream's spec.md on disk, as the owner does.
func (f *shedFixture) editSpec(t *testing.T, stream config.WorkstreamID, content string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(f.trace, "workstreams", string(stream), plan.SpecPath), []byte(content), 0600))
}

// blocks returns the reasons the mason controller recorded the unit of the
// workstream blocked for, checking each has a notice for the chief of staff.
func (f *shedFixture) blocks(t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	var out []string
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.Subject != blockedSubject(unit) {
			continue
		}
		if tr.Actor != masonActor || tr.ID != fmt.Sprintf("%s-%d", blockedSubject(unit), len(out)+1) || tr.To != fmt.Sprintf("blocked-%d", len(out)+1) {
			t.Fatalf("blocked transition %+v", tr)
		}
		if body := f.notice(t, stream, tr.ID); body != "The mason controller is blocked: "+tr.Reason+". It tries again on every pass." {
			t.Fatalf("notice %q", body)
		}
		out = append(out, tr.Reason)
	}
	return out
}

// staleReason is the reason a unit is blocked for when spec.md revision 2
// is staleSpec.
func staleReason(prefix string) string {
	return prefix + ": its mason bundle cannot be assembled: spec does not match its seal: spec.md revision 2 hashes to " + seal.SpecHash(staleSpec) + ", seal 1 records " + seal.SpecHash(validSpec)
}

// A ready unit whose spec no longer matches its seal is not started: it
// stays ready with no workspace and no mason thread, and why is recorded
// once, with a notice for the chief of staff, however many passes find it
// so. Once the owner puts the sealed spec back, the unit starts.
func TestStaleSpecLeavesTheUnitReady(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, validPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	stream, _ := f.builtAs(t, "design")
	f.editSpec(t, stream, staleSpec)
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonTransition(t, stream)
	// Further passes find the unit still blocked and record nothing more.
	settle()
	settle()

	if got := masonTransitions(t, f, stream); len(got) != 1 || got[0].Subject != blockedSubject("resume") {
		t.Fatalf("mason transitions %+v", got)
	}
	if got, want := f.blocks(t, stream, "resume"), []string{staleReason("unit resume stays ready")}; !slices.Equal(got, want) {
		t.Fatalf("blocked %q, want %q", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitPlanned}})
	if _, err := os.Stat(filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the blocked unit has a workspace: %v", err)
	}
	if _, err := f.repository().Thread(stream, masonAgent("resume")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the blocked unit has a mason thread: %v", err)
	}

	f.editSpec(t, stream, validSpec)
	f.awaitMasonRan(t, stream, "resume")
	masons.check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitPlanned}})
}

// lowHigh returns the two workstreams in workstream ID order, the order in
// which workstreams equal in priority are offered a mason slot.
func lowHigh(a, b config.WorkstreamID) (config.WorkstreamID, config.WorkstreamID) {
	if b < a {
		return b, a
	}
	return a, b
}

// A unit whose workspace cannot be opened, here because a directory the
// clone does not know is in its place, stays ready and is recorded blocked,
// and the service keeps running: the mason slot goes to the next workstream,
// whose unit starts.
func TestUnitWorkspaceFailureBlocksItsWorkstreamAlone(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	a, _ := f.builtAs(t, "first")
	b, _ := f.builtAs(t, "second")
	broken, other := lowHigh(a, b)
	squatter := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(broken), "resume")
	must(t, os.MkdirAll(squatter, 0700))
	must(t, os.WriteFile(filepath.Join(squatter, "notes"), []byte("mine\n"), 0600))
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, other, "resume")
	settle()
	masons.check(t)

	reasons := f.blocks(t, broken, "resume")
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "unit resume stays ready: its workspace cannot be opened: ") {
		t.Fatalf("blocked %q", reasons)
	}
	f.checkUnits(t, broken, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitPlanned}})
	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitPlanned}})
	if data, err := os.ReadFile(filepath.Join(squatter, "notes")); err != nil || string(data) != "mine\n" {
		t.Fatalf("the directory in the workspace's place changed: %q %v", data, err)
	}
	if _, err := f.c.Health(context.Background()); err != nil {
		t.Fatalf("the service stopped: %v", err)
	}
}

// An implementing unit whose first mason turn is not queued gets no turn
// when its spec no longer matches its seal or its workspace cannot be
// opened: it is recorded blocked and holds no mason slot, which goes to the
// next workstream.
func TestBlockedImplementingUnitHoldsNoSlot(t *testing.T) {
	t.Parallel()
	const waits = "unit resume is implementing and its mason's first turn is not queued"
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, f *shedFixture, stream config.WorkstreamID)
		check func(reason string) bool
	}{
		{"stale spec", func(t *testing.T, f *shedFixture, stream config.WorkstreamID) { f.editSpec(t, stream, staleSpec) },
			func(reason string) bool { return reason == staleReason(waits) }},
		{"workspace", func(t *testing.T, f *shedFixture, stream config.WorkstreamID) {
			squatter := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
			must(t, os.MkdirAll(squatter, 0700))
			must(t, os.WriteFile(filepath.Join(squatter, "notes"), []byte("mine\n"), 0600))
		}, func(reason string) bool { return strings.HasPrefix(reason, waits+": its workspace cannot be opened: ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, masons := newMasonFixture(t, 1, validPlan)
			defer func() { f.stop(t) }()
			factory := runtime.Target{Scope: "factory"}
			mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
			a, _ := f.builtAs(t, "first")
			b, _ := f.builtAs(t, "second")
			blocked, other := lowHigh(a, b)
			f.stop(t)

			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			states, err := repo.WorkflowStates(blocked)
			must(t, err)
			subject := trace.UnitSubject("resume")
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: masonTransitionID("resume"), Revision: 1, Project: f.project, Workstream: blocked, Unit: "resume", At: f.clock.Now(), Actor: masonActor, Cause: subject + "-" + UnitReady}
			_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: states[subject].Version, Transition: trace.Transition{Header: h, Subject: subject, From: UnitReady, To: UnitImplementing, Reason: "planted"}})
			must(t, errors.Join(err, repo.Close()))
			tc.plant(t, f, blocked)

			f.start(t)
			mutation(t, f.c, "DELETE", "pause", factory)
			f.awaitMasonRan(t, other, "resume")
			settle()
			masons.check(t)
			if got := f.blocks(t, blocked, "resume"); len(got) != 1 || !tc.check(got[0]) {
				t.Fatalf("blocked %q", got)
			}
			if _, err := f.repository().Thread(blocked, masonAgent("resume")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the blocked unit has a mason thread: %v", err)
			}
			f.checkUnits(t, blocked, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitPlanned}})
		})
	}
}

// Units are taken in the plan's dependency order: a unit follows the units
// it depends on, and otherwise keeps its place in the plan.
func TestNextReadyFollowsTheDependencyOrder(t *testing.T) {
	t.Parallel()
	unit := func(id string, deps ...string) plan.Unit { return plan.Unit{ID: id, DependsOn: deps} }
	p := plan.Plan{Units: []plan.Unit{unit("c", "a"), unit("b"), unit("a"), unit("d")}}
	states := func(ready ...string) map[string]trace.WorkflowState {
		out := map[string]trace.WorkflowState{trace.UnitSubject("a"): {Value: UnitMerged}}
		for _, id := range ready {
			out[trace.UnitSubject(id)] = trace.WorkflowState{Value: UnitReady}
		}
		return out
	}
	for _, tc := range []struct {
		ready []string
		want  string
	}{{[]string{"c", "b"}, "b"}, {[]string{"c", "d"}, "c"}, {[]string{"d"}, "d"}, {nil, ""}} {
		got, ok := nextReady(building{plan: p, states: states(tc.ready...)})
		if got != tc.want || ok != (tc.want != "") {
			t.Fatalf("ready %v: next %q %v, want %q", tc.ready, got, ok, tc.want)
		}
	}
}

// Workstreams are offered a slot in the project's priority order, those it
// does not name last; among equals, the one that started a unit least
// recently goes first, then the lower ID.
func TestStartOrderIsPriorityThenRoundRobin(t *testing.T) {
	t.Parallel()
	at := func(minutes int) time.Time { return demoStart.Add(time.Duration(minutes) * time.Minute) }
	streams := []building{{stream: "a", started: at(2)}, {stream: "b", started: at(1)}, {stream: "c", started: at(3)}, {stream: "d"}, {stream: "e"}}
	priorities := []runtime.Priority{{Project: "other", Workstreams: []config.WorkstreamID{"a"}}, {Project: project, Workstreams: []config.WorkstreamID{"c"}}}
	var got []config.WorkstreamID
	for _, b := range startOrder(streams, priorities, project) {
		got = append(got, b.stream)
	}
	if want := []config.WorkstreamID{"c", "d", "e", "b", "a"}; !slices.Equal(got, want) {
		t.Fatalf("start order %v, want %v", got, want)
	}
}
