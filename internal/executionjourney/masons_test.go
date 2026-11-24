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
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func implementationRuns(m *intakefixture.FakeMasons, stream config.WorkstreamID) []agent.Request {
	return slices.DeleteFunc(m.Requests(stream), func(req agent.Request) bool {
		return !strings.HasSuffix(req.Name, "-implement")
	})
}

func masonTransitions(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) []intakefixture.TransitionMove {
	t.Helper()
	var out []intakefixture.TransitionMove
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Actor == intake.MasonActor && (strings.HasPrefix(tr.Subject, "unit-") || strings.HasPrefix(tr.Subject, "unit_") || strings.HasPrefix(tr.Subject, "blocked-mason")) {
			out = append(out, intakefixture.TransitionMove{ID: tr.ID, Subject: tr.Subject, From: tr.From, To: tr.To, Cause: tr.Cause, Reason: tr.Reason})
		}
	}
	return out
}

func awaitMasonTransition(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.Soon(t, "the mason controller of "+string(stream)+" to record a transition", func() bool {
		return len(masonTransitions(t, f, stream)) != 0
	})
}

func startedReason(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit string) string {
	t.Helper()
	base := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream)))
	return fmt.Sprintf("unit %s is the next ready unit of the plan of seal 1; its mason works in the unit's workspace on %s, created from %s at %s", unit, execution.UnitBranch(stream, unit), intake.FeatureBranch(stream), base)
}

func started(unit, reason string) intakefixture.TransitionMove {
	return intakefixture.TransitionMove{ID: execution.MasonTransitionID(unit), Subject: trace.UnitSubject(unit), From: intake.UnitReady, To: intake.UnitImplementing, Cause: trace.UnitSubject(unit) + "-" + intake.UnitReady, Reason: reason}
}

func awaitAcknowledgedNotices(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.Soon(t, "the notices of "+string(stream)+" to be acknowledged", func() bool {
		outbox, err := f.Repository().Outbox(stream)
		fixturekit.Must(t, err)
		return !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool { return intakefixture.ChiefEvent(e.Event) && !e.Acknowledged })
	})
}

func TestMasonStartsTheFirstOfTwoEntangledUnits(t *testing.T) {
	t.Parallel()
	passed, prepare := journey.CountingSchedule()
	f, masons := journey.NewMasonFixturePrepared(t, 4, intakefixture.IndependentPlan, prepare)
	defer f.Stop(t)
	stream := seedBuilding(f, t, "design", intakefixture.IndependentPlan)
	f.AwaitMasonRan(t, stream, "resume")

	intakefixture.AwaitPasses(t, passed, 3)
	masons.Check(t)

	if got, want := masonTransitions(t, f, stream), []intakefixture.TransitionMove{started("resume", startedReason(f, t, stream, "resume"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	startNotice := "Unit resume is implementing: its mason works on it in its unit workspace on " + execution.UnitBranch(stream, "resume") + "."
	if body := f.Notice(t, stream, execution.MasonTransitionID("resume")); body != startNotice {
		t.Fatalf("the start notice %q, want %q", body, startNotice)
	}
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, stream, "dedupe", overlapping("resume"))})

	f.AwaitEventTurns(t, stream, startNotice)
	awaitAcknowledgedNotices(f, t, stream)

	th, err := f.Repository().Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	if len(th.Turns) < 1 || th.Identity.Role != intake.MasonRole || th.Identity.Unit != "resume" {
		t.Fatalf("mason thread %+v", th)
	}
	req := th.Turns[0].Request
	if req.TurnID != execution.MasonTurnID("resume") || req.Unit != "resume" || req.Cause != execution.MasonTransitionID("resume") || req.Actor != intake.MasonActor {
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

	workspace := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume")
	if run.Workspace.Directory() == workspace {
		t.Fatal("the mason turn ran in the workspace itself, not a view of it")
	}
	if data, err := os.ReadFile(filepath.Join(workspace, fixturekit.MasonWrote)); err != nil || string(data) != "package trace\n" {
		t.Fatalf("what the mason wrote is not in the unit's workspace: %q %v", data, err)
	}
	if branch := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", workspace, "branch", "--show-current")); branch != execution.UnitBranch(stream, "resume") {
		t.Fatalf("the unit's workspace is on %s", branch)
	}
	if _, err := os.Stat(filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "dedupe")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the waiting unit has a workspace: %v", err)
	}
}

func TestImplementingUnitGetsItsMasonTurnAfterARestart(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewMasonFixture(t, 4, fixturekit.ValidPlan)
	defer func() { f.Stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	stream := seedBuildingPaused(f, t, factory, fixturekit.ValidPlan, "design")[0]
	f.Stop(t)

	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	states, err := repo.WorkflowStates(stream)
	fixturekit.Must(t, err)
	subject := trace.UnitSubject("resume")
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: execution.MasonTransitionID("resume"), Revision: 1, Project: f.Project, Workstream: stream, Unit: "resume", At: f.Clock.Now(), Actor: intake.MasonActor, Cause: subject + "-" + intake.UnitReady}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: states[subject].Version, Transition: trace.Transition{Header: h, Subject: subject, From: intake.UnitReady, To: intake.UnitImplementing, Reason: "planted"}})
	fixturekit.Must(t, errors.Join(err, repo.Close()))

	f.Start(t)
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	f.AwaitMasonRan(t, stream, "resume")
	masons.Check(t)
	if runs := implementationRuns(masons, stream); len(runs) != 1 || !strings.Contains(runs[0].Prompt, "# Unit resume\n") {
		t.Fatalf("initial mason turns: got %d", len(runs))
	}
	if got := masonTransitions(t, f, stream); len(got) != 1 || got[0].Reason != "planted" {
		t.Fatalf("mason transitions %+v", got)
	}
	workspace := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume")
	if _, err := os.Stat(filepath.Join(workspace, fixturekit.MasonWrote)); err != nil {
		t.Fatal(err)
	}
}

const staleSpec = fixturekit.ValidSpec + "9. Something new.\n"

func editSpec(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, content string) {
	t.Helper()
	if active := f.S.Sole(); active != nil {
		intakefixture.RecordSpec(t, active.Repository, stream, content)
		return
	}
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	intakefixture.RecordSpec(t, repo, stream, content)
}

func blocks(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	var out []string
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Subject != execution.BlockedSubject(unit) {
			continue
		}
		if tr.Actor != intake.MasonActor || tr.ID != fmt.Sprintf("%s-%d", execution.BlockedSubject(unit), len(out)+1) || tr.To != fmt.Sprintf("blocked-%d", len(out)+1) {
			t.Fatalf("blocked transition %+v", tr)
		}
		if body := f.Notice(t, stream, tr.ID); body != "The mason controller is blocked: "+tr.Reason+". It tries again on every pass." {
			t.Fatalf("notice %q", body)
		}
		out = append(out, tr.Reason)
	}
	return out
}

func staleReason(prefix string) string {
	return prefix + ": its mason bundle cannot be assembled: spec does not match its seal: spec.md revision 2 hashes to " + seal.SpecHash(staleSpec) + ", seal 1 records " + seal.SpecHash(fixturekit.ValidSpec)
}

func TestStaleSpecLeavesTheUnitReady(t *testing.T) {
	t.Parallel()
	passed, prepare := journey.CountingSchedule()
	f, masons := journey.NewMasonFixturePrepared(t, 4, fixturekit.ValidPlan, prepare)
	defer f.Stop(t)
	factory := runtime.Target{Scope: "factory"}
	stream := seedBuildingPaused(f, t, factory, fixturekit.ValidPlan, "design")[0]
	editSpec(f, t, stream, staleSpec)
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	awaitMasonTransition(f, t, stream)

	intakefixture.AwaitPasses(t, passed, 6)

	if got := masonTransitions(t, f, stream); len(got) != 1 || got[0].Subject != execution.BlockedSubject("resume") {
		t.Fatalf("mason transitions %+v", got)
	}
	if got, want := blocks(f, t, stream, "resume"), []string{staleReason("unit resume stays ready")}; !slices.Equal(got, want) {
		t.Fatalf("blocked %q, want %q", got, want)
	}
	f.CheckUnits(t, stream, []api.UnitStatus{deferred(f, t, stream, "resume", blockedOnItself("resume")), {Unit: "dedupe", State: intake.UnitPlanned}})
	if _, err := os.Stat(filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the blocked unit has a workspace: %v", err)
	}
	if _, err := f.Repository().Thread(stream, intake.MasonAgent("resume")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the blocked unit has a mason thread: %v", err)
	}

	editSpec(f, t, stream, fixturekit.ValidSpec)
	f.AwaitMasonRan(t, stream, "resume")
	masons.Check(t)
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, {Unit: "dedupe", State: intake.UnitPlanned}})
}

func TestUnitWorkspaceFailureBlocksItsWorkstreamAlone(t *testing.T) {
	t.Parallel()
	passed, prepare := journey.CountingSchedule()
	f, masons := journey.NewMasonFixturePrepared(t, 1, fixturekit.ValidPlan, prepare)
	defer f.Stop(t)
	factory := runtime.Target{Scope: "factory"}
	built := seedBuildingPaused(f, t, factory, fixturekit.ValidPlan, "first", "second")
	a, b := built[0], built[1]
	broken, other := intakefixture.LowHigh(a, b)
	squatter := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(broken), "resume")
	fixturekit.Must(t, os.MkdirAll(squatter, 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(squatter, "notes"), []byte("mine\n"), 0600))

	intakefixture.AwaitPasses(t, passed, 3)
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	f.AwaitMasonRan(t, other, "resume")

	intakefixture.AwaitPasses(t, passed, 3)
	masons.Check(t)

	reasons := blocks(f, t, broken, "resume")
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "unit resume stays ready: its workspace cannot be opened: ") {
		t.Fatalf("blocked %q", reasons)
	}

	if !slices.Contains(dispatches(f, t, broken, "resume"), "deferred "+execution.DeferBlocked) {
		t.Fatalf("resume of %s was never recorded blocked: %v", broken, dispatches(f, t, broken, "resume"))
	}
	current := blockedOnItself("resume")
	if d := journey.UnitStatus(t, f, broken, "resume").Deferral; d != nil && d.Reason == execution.DeferCapacity {
		current = slotless(1)
	}
	f.CheckUnits(t, broken, []api.UnitStatus{deferred(f, t, broken, "resume", current), {Unit: "dedupe", State: intake.UnitPlanned}})
	f.CheckUnits(t, other, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, {Unit: "dedupe", State: intake.UnitPlanned}})
	if data, err := os.ReadFile(filepath.Join(squatter, "notes")); err != nil || string(data) != "mine\n" {
		t.Fatalf("the directory in the workspace's place changed: %q %v", data, err)
	}
	if _, err := f.C.Health(context.Background()); err != nil {
		t.Fatalf("the service stopped: %v", err)
	}
}

func TestBlockedImplementingUnitHoldsNoSlot(t *testing.T) {
	t.Parallel()
	const waits = "unit resume is implementing and its mason's first turn is not queued"
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID)
		check func(reason string) bool
	}{
		{"stale spec", func(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) {
			editSpec(f, t, stream, staleSpec)
		},
			func(reason string) bool { return reason == staleReason(waits) }},
		{"workspace", func(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) {
			squatter := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume")
			fixturekit.Must(t, os.MkdirAll(squatter, 0700))
			fixturekit.Must(t, os.WriteFile(filepath.Join(squatter, "notes"), []byte("mine\n"), 0600))
		}, func(reason string) bool { return strings.HasPrefix(reason, waits+": its workspace cannot be opened: ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			passed, prepare := journey.CountingSchedule()
			f, masons := journey.NewMasonFixturePrepared(t, 1, fixturekit.ValidPlan, prepare)
			defer func() { f.Stop(t) }()
			factory := runtime.Target{Scope: "factory"}
			built := seedBuildingPaused(f, t, factory, fixturekit.ValidPlan, "first", "second")
			a, b := built[0], built[1]
			blocked, other := intakefixture.LowHigh(a, b)
			f.Stop(t)

			repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			plantImplementing(t, f, repo, blocked, "resume")
			fixturekit.Must(t, repo.Close())
			tc.plant(t, f, blocked)

			f.Start(t)
			intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
			f.AwaitMasonRan(t, other, "resume")

			intakefixture.AwaitPasses(t, passed, 3)
			masons.Check(t)
			if got := blocks(f, t, blocked, "resume"); len(got) != 1 || !tc.check(got[0]) {
				t.Fatalf("blocked %q", got)
			}
			if _, err := f.Repository().Thread(blocked, intake.MasonAgent("resume")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the blocked unit has a mason thread: %v", err)
			}
			f.CheckUnits(t, blocked, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, {Unit: "dedupe", State: intake.UnitPlanned}})
		})
	}
}

func plantImplementing(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	states, err := repo.WorkflowStates(stream)
	fixturekit.Must(t, err)
	subject := trace.UnitSubject(unit)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: execution.MasonTransitionID(unit), Revision: 1, Project: f.Project, Workstream: stream, Unit: unit, At: f.Clock.Now(), Actor: intake.MasonActor, Cause: subject + "-" + intake.UnitReady}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: states[subject].Version, Transition: trace.Transition{Header: h, Subject: subject, From: intake.UnitReady, To: intake.UnitImplementing, Reason: "planted"}})
	fixturekit.Must(t, err)
}

type stoppedAfter struct{ context.Context }

func (stoppedAfter) Done() <-chan struct{} { return nil }

func (stoppedAfter) Err() error { return context.Canceled }

func TestStopWhileOpeningAUnitWorkspaceEndsThePass(t *testing.T) {
	t.Parallel()
	f, _ := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	stream := seedBuildingPaused(f, t, runtime.Target{Scope: "factory"}, fixturekit.ValidPlan, "first")[0]
	f.Stop(t)
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	plantImplementing(t, f, repo, stream, "resume")
	squatter := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume")
	fixturekit.Must(t, os.MkdirAll(squatter, 0700))

	m := &journey.Masons{S: f.S, Cfg: f.S.Cfg, Repository: repo}
	if _, err := m.Driver().Resume(stoppedAfter{context.Background()}, stream, "resume"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resume during a stop: %v", err)
	}
	if state, err := repo.Workflow(stream, execution.BlockedSubject("resume")); err != nil || state.Value != "" {
		t.Fatalf("a stopped pass recorded a block: %+v %v", state, err)
	}
}
