package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	fs "io/fs"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"

	plan "github.com/kpenfound/osmia/internal/plan"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"

	atomic "sync/atomic"
	testing "testing"
	time "time"
)

const masonRoles = chiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

type fakeMasons = intakefixture.FakeMasons

var sessionStream = intakefixture.SessionStream

var errFailTurn = intakefixture.ErrFailTurn

var failMasonTurn = intakefixture.FailMasonTurn

func implementationRuns(m *fakeMasons, stream config.WorkstreamID) []agent.Request {
	return slices.DeleteFunc(m.Requests(stream), func(req agent.Request) bool {
		return !strings.HasSuffix(req.Name, "-implement")
	})
}

func newMasonFixture(t *testing.T, masons int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newCappedMasonFixture(t, fmt.Sprintf("masons = %d\n", masons), drafted)
}

func newCappedMasonFixture(t *testing.T, capacity, drafted string) (*shedFixture, *fakeMasons) {
	return newConfiguredMasonFixture(t, capacity, drafted, "")
}

func newConfiguredMasonFixture(t *testing.T, capacity, drafted, classifier string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newMasonFixtureOn(t, config.WorkspacesGit, capacity, drafted, classifier)
}

func newMasonFixtureOn(t *testing.T, backend, capacity, drafted, classifier string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, backend, capacity, drafted, classifier, nil)
}

func newMasonFixturePrepared(t *testing.T, masons int, drafted string, prepare func(*Options)) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, config.WorkspacesGit, fmt.Sprintf("masons = %d\n", masons), drafted, "", prepare)
}

func countingSchedule() (*atomic.Int64, func(*Options)) {
	passed, scheduled := intakefixture.ScheduleCounter()
	return passed, func(opts *Options) { opts.schedulePassed = scheduled }
}

func newMasonFixtureWith(t *testing.T, backend, capacity, drafted, classifier string, prepare func(*Options)) (*shedFixture, *fakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, masonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, backend, capacity, classifier, opts.Threads)
		if prepare != nil {
			prepare(opts)
		}
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: drafted}, nil)
	f.upstream(t)
	return f, intakefixture.ScriptMasons(f.engine, masonTurnID)
}

func masonTransitions(t *testing.T, f *shedFixture, stream config.WorkstreamID) []transitionMove {
	t.Helper()
	var out []transitionMove
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.Actor == masonActor && (strings.HasPrefix(tr.Subject, "unit-") || strings.HasPrefix(tr.Subject, "unit_") || strings.HasPrefix(tr.Subject, "blocked-mason")) {
			out = append(out, transitionMove{ID: tr.ID, Subject: tr.Subject, From: tr.From, To: tr.To, Cause: tr.Cause, Reason: tr.Reason})
		}
	}
	return out
}

func (f *shedFixture) awaitMasonRan(t *testing.T, stream config.WorkstreamID, unit string) trace.Thread {
	t.Helper()
	return f.sharedTurns().AwaitMasonRan(t, stream, unit)
}

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

func (f *shedFixture) startedReason(t *testing.T, stream config.WorkstreamID, unit string) string {
	t.Helper()
	base := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", featureBranch(stream)))
	return fmt.Sprintf("unit %s is the next ready unit of the plan of seal 1; its mason works in the unit's workspace on %s, created from %s at %s", unit, unitBranch(stream, unit), featureBranch(stream), base)
}

func started(unit, reason string) transitionMove {
	return transitionMove{ID: masonTransitionID(unit), Subject: trace.UnitSubject(unit), From: UnitReady, To: UnitImplementing, Cause: trace.UnitSubject(unit) + "-" + UnitReady, Reason: reason}
}

var awaitPasses = intakefixture.AwaitPasses

func (f *shedFixture) awaitEventTurns(t *testing.T, stream config.WorkstreamID, want ...string) {
	t.Helper()
	intakefixture.AwaitEventTurns(t, f.repository(), stream, want...)
}

func (f *shedFixture) awaitAcknowledgedNotices(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		outbox, err := f.repository().Outbox(stream)
		must(t, err)
		pending := 0
		for _, e := range outbox {
			if chiefEvent(e.Event) && !e.Acknowledged {
				pending++
			}
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d notices of %s were never acknowledged: %+v", pending, stream, outbox)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMasonStartsTheFirstOfTwoEntangledUnits(t *testing.T) {
	t.Parallel()
	passed, prepare := countingSchedule()
	f, masons := newMasonFixturePrepared(t, 4, independentPlan, prepare)
	defer f.stop(t)
	stream := f.seedBuilding(t, "design", independentPlan)
	f.awaitMasonRan(t, stream, "resume")

	awaitPasses(t, passed, 3)
	masons.Check(t)

	if got, want := masonTransitions(t, f, stream), []transitionMove{started("resume", f.startedReason(t, stream, "resume"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	startNotice := "Unit resume is implementing: its mason works on it in its unit workspace on " + unitBranch(stream, "resume") + "."
	if body := f.notice(t, stream, masonTransitionID("resume")); body != startNotice {
		t.Fatalf("the start notice %q, want %q", body, startNotice)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, stream, "dedupe", overlapping("resume"))})

	f.awaitEventTurns(t, stream, startNotice)
	f.awaitAcknowledgedNotices(t, stream)

	th, err := f.repository().Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(th.Turns) < 1 || th.Identity.Role != masonRole || th.Identity.Unit != "resume" {
		t.Fatalf("mason thread %+v", th)
	}
	req := th.Turns[0].Request
	if req.TurnID != masonTurnID("resume") || req.Unit != "resume" || req.Cause != masonTransitionID("resume") || req.Actor != masonActor {
		t.Fatalf("mason turn request %+v", req)
	}
	runs := masons.Requests(stream)
	if len(runs) < 1 {
		t.Fatalf("mason turns run %d times", len(runs))
	}
	run := runs[0]
	for _, want := range []string{"Build unit resume of this workstream.", "# Unit resume\n", "title: Resume from the last chunk\n", "seal: 1\n", "## Task\nResume from the last chunk\n", "## Acceptance\n- spec#1 holds, shown by TestResume (new-test)\n", "- spec#1: ", "## Spec\n", "call done with the outcome of your work: what it now does and how you checked the acceptance", "Add a short headline, what happened in concrete terms, and needs_you only when the owner has a specific action.", "Then end your turn."} {
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

func TestImplementingUnitGetsItsMasonTurnAfterARestart(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, validPlan)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	stream := f.seedBuildingPaused(t, factory, validPlan, "design")[0]
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
	masons.Check(t)
	if runs := implementationRuns(masons, stream); len(runs) != 1 || !strings.Contains(runs[0].Prompt, "# Unit resume\n") {
		t.Fatalf("initial mason turns: got %d", len(runs))
	}
	if got := masonTransitions(t, f, stream); len(got) != 1 || got[0].Reason != "planted" {
		t.Fatalf("mason transitions %+v", got)
	}
	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
	if _, err := os.Stat(filepath.Join(workspace, masonWrote)); err != nil {
		t.Fatal(err)
	}
}

const staleSpec = validSpec + "9. Something new.\n"

func (f *shedFixture) editSpec(t *testing.T, stream config.WorkstreamID, content string) {
	t.Helper()
	if active := f.s.sole(); active != nil {
		recordSpec(t, active.repository, stream, content)
		return
	}
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	recordSpec(t, repo, stream, content)
}

func (f *shedFixture) blocks(t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	var out []string
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
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

func staleReason(prefix string) string {
	return prefix + ": its mason bundle cannot be assembled: spec does not match its seal: spec.md revision 2 hashes to " + seal.SpecHash(staleSpec) + ", seal 1 records " + seal.SpecHash(validSpec)
}

func TestStaleSpecLeavesTheUnitReady(t *testing.T) {
	t.Parallel()
	passed, prepare := countingSchedule()
	f, masons := newMasonFixturePrepared(t, 4, validPlan, prepare)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	stream := f.seedBuildingPaused(t, factory, validPlan, "design")[0]
	f.editSpec(t, stream, staleSpec)
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonTransition(t, stream)

	awaitPasses(t, passed, 6)

	if got := masonTransitions(t, f, stream); len(got) != 1 || got[0].Subject != blockedSubject("resume") {
		t.Fatalf("mason transitions %+v", got)
	}
	if got, want := f.blocks(t, stream, "resume"), []string{staleReason("unit resume stays ready")}; !slices.Equal(got, want) {
		t.Fatalf("blocked %q, want %q", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{f.deferred(t, stream, "resume", blockedOnItself("resume")), {Unit: "dedupe", State: UnitPlanned}})
	if _, err := os.Stat(filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the blocked unit has a workspace: %v", err)
	}
	if _, err := f.repository().Thread(stream, masonAgent("resume")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the blocked unit has a mason thread: %v", err)
	}

	f.editSpec(t, stream, validSpec)
	f.awaitMasonRan(t, stream, "resume")
	masons.Check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitPlanned}})
}

var lowHigh = intakefixture.LowHigh

func TestUnitWorkspaceFailureBlocksItsWorkstreamAlone(t *testing.T) {
	t.Parallel()
	passed, prepare := countingSchedule()
	f, masons := newMasonFixturePrepared(t, 1, validPlan, prepare)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	built := f.seedBuildingPaused(t, factory, validPlan, "first", "second")
	a, b := built[0], built[1]
	broken, other := lowHigh(a, b)
	squatter := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(broken), "resume")
	must(t, os.MkdirAll(squatter, 0700))
	must(t, os.WriteFile(filepath.Join(squatter, "notes"), []byte("mine\n"), 0600))
	// Let a paused pass observe both builds before either can take the slot.
	awaitPasses(t, passed, 3)
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, other, "resume")

	awaitPasses(t, passed, 3)
	masons.Check(t)

	reasons := f.blocks(t, broken, "resume")
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "unit resume stays ready: its workspace cannot be opened: ") {
		t.Fatalf("blocked %q", reasons)
	}

	if !slices.Contains(f.dispatches(t, broken, "resume"), "deferred "+DeferBlocked) {
		t.Fatalf("resume of %s was never recorded blocked: %v", broken, f.dispatches(t, broken, "resume"))
	}
	current := blockedOnItself("resume")
	if d := unitStatus(t, f, broken, "resume").Deferral; d != nil && d.Reason == DeferCapacity {
		current = slotless(1)
	}
	f.checkUnits(t, broken, []UnitStatus{f.deferred(t, broken, "resume", current), {Unit: "dedupe", State: UnitPlanned}})
	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitPlanned}})
	if data, err := os.ReadFile(filepath.Join(squatter, "notes")); err != nil || string(data) != "mine\n" {
		t.Fatalf("the directory in the workspace's place changed: %q %v", data, err)
	}
	if _, err := f.c.Health(context.Background()); err != nil {
		t.Fatalf("the service stopped: %v", err)
	}
}

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
			passed, prepare := countingSchedule()
			f, masons := newMasonFixturePrepared(t, 1, validPlan, prepare)
			defer func() { f.stop(t) }()
			factory := runtime.Target{Scope: "factory"}
			built := f.seedBuildingPaused(t, factory, validPlan, "first", "second")
			a, b := built[0], built[1]
			blocked, other := lowHigh(a, b)
			f.stop(t)

			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			plantImplementing(t, f, repo, blocked, "resume")
			must(t, repo.Close())
			tc.plant(t, f, blocked)

			f.start(t)
			mutation(t, f.c, "DELETE", "pause", factory)
			f.awaitMasonRan(t, other, "resume")

			awaitPasses(t, passed, 3)
			masons.Check(t)
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

func plantImplementing(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	states, err := repo.WorkflowStates(stream)
	must(t, err)
	subject := trace.UnitSubject(unit)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: masonTransitionID(unit), Revision: 1, Project: f.project, Workstream: stream, Unit: unit, At: f.clock.Now(), Actor: masonActor, Cause: subject + "-" + UnitReady}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: states[subject].Version, Transition: trace.Transition{Header: h, Subject: subject, From: UnitReady, To: UnitImplementing, Reason: "planted"}})
	must(t, err)
}

type stoppedAfter struct{ context.Context }

func (stoppedAfter) Done() <-chan struct{} { return nil }

func (stoppedAfter) Err() error { return context.Canceled }

func TestStopWhileOpeningAUnitWorkspaceEndsThePass(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, validPlan)
	stream := f.seedBuildingPaused(t, runtime.Target{Scope: "factory"}, validPlan, "first")[0]
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	plantImplementing(t, f, repo, stream, "resume")
	squatter := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
	must(t, os.MkdirAll(squatter, 0700))

	m := &masons{s: f.s, cfg: f.s.cfg, repository: repo}
	if _, err := m.resume(stoppedAfter{context.Background()}, stream, "resume"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resume during a stop: %v", err)
	}
	if state, err := repo.Workflow(stream, blockedSubject("resume")); err != nil || state.Value != "" {
		t.Fatalf("a stopped pass recorded a block: %+v %v", state, err)
	}
}
