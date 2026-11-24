package lifecycle_test

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionQueuedDeltaRetiresBeforeExecution(t *testing.T) {
	t.Parallel()
	for _, unavailable := range []bool{false, true} {
		name := "unchanged-retired-slice"
		if unavailable {
			name = "changed-retired-slice-refused"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testQueuedDeltaRetirement(t, unavailable)
		})
	}
}

func testQueuedDeltaRetirement(t *testing.T, unavailable bool) {
	ctx := context.Background()
	queued := &intakefixture.ChangeRaceGate{Step: "change-delta-queued", Reached: make(chan struct{}), Released: make(chan struct{})}
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(queued.Release)
	t.Cleanup(cleanup.release)
	var allowedAddress atomic.Value
	allowedAddress.Store(config.WorkstreamID(""))
	var allowSurvivor atomic.Bool
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, options: func(opts *Options) {
		limit := opts.Reconciliation.Limit
		opts.Reconciliation.Limit = func(op coreadapter.Operation) (func(), bool) {
			if op.Action == lifecycle.DeltaTurnAction && !allowSurvivor.Load() {
				in, err := lifecycle.DecodeDeltaTurn(op)
				if err != nil || in.Address != allowedAddress.Load().(config.WorkstreamID) {
					return nil, false
				}
			}
			if limit != nil {
				return limit(op)
			}
			return nil, true
		}
	}, checkpoint: func(ctx context.Context, step string) error {
		if err := queued.Checkpoint(ctx, step); err != nil {
			return err
		}
		return cleanup.checkpoint(ctx, step)
	}}, "", nil)
	defer f.stop(t)
	var drafts atomic.Int32
	var address config.WorkstreamID
	installRetirementDelta(t, f, &drafts, func(after *trace.ProjectSpec) {
		if unavailable {
			for i := range after.Slices {
				if after.Slices[i].Workstream == address {
					after.Slices[i].Spec += "\nAdditional handling preserves offsets.\n"
				}
			}
		}
	})
	queued.Armed.Store(true)
	c, apiFault := f.s.requestChange(ctx, string(f.origin), lifecycle.ChangeRequest{Key: "retired-dispatch", Kind: "amend", Scope: "workgroup", Change: "Revise the shared acknowledgment contract."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	queued.Await(t)
	var owner *trace.Repository
	var intent trace.OperationRecord
	for _, m := range resolutionMembers(f) {
		repo, err := f.s.repository(m.project)
		fixturekit.Must(t, err)
		ops, err := repo.Operations(m.stream)
		fixturekit.Must(t, err)
		for _, op := range ops {
			if op.Operation.Action == lifecycle.DeltaTurnAction {
				if address != "" {
					t.Fatal("more than one delta dispatch")
				}
				address, owner, intent = m.stream, repo, op
			}
		}
	}
	if address == "" || intent.Claim != nil || intent.Result != nil {
		t.Fatalf("delta was not queued before execution: %+v", intent)
	}
	before := currentChange(t, f.s, f.origin, c.ID)
	awaitMemberBuilt(t, f, owner, address)
	_, err := f.c.Abandon(ctx, address, "Owner cancelled the member before its queued delta ran.")
	fixturekit.Must(t, err)
	_, err = f.c.Archive(ctx, address)
	fixturekit.Must(t, err)
	observed, err := (deltaRunner{s: f.s}).Inspect(ctx, intent.Operation)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectCompleted || observed.Result == nil || observed.Result.Outcome != "cancelled" {
		t.Fatalf("retired queued intent: %+v", observed)
	}
	survivor := f.stream1
	if survivor == address {
		survivor = f.stream2
	}
	if after := currentChange(t, f.s, survivor, c.ID); !intakefixture.EqualJSONTest(before, after) {
		t.Fatal("retirement inspection changed the authoritative journal")
	}
	attempt, _, err := owner.BeginOperation(ctx, address, intent.EventID, trace.WorkerOperation, trace.Actor{Kind: "service", ID: "retired-delta-retry"}, f.clock.Now)
	fixturekit.Must(t, err)
	if attempt == nil {
		t.Fatal("could not force the retired dispatch retry")
	}
	retry := attempt.Action("retry", f.clock.Now())
	retry.Failure = "retired dispatch retry before cancellation inspection"
	retry.RetryAt = retry.At.Add(time.Second)
	err = attempt.Record(ctx, retry)
	attempt.Close()
	fixturekit.Must(t, err)
	ops, err := owner.Operations(address)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
		return op.Operation.ID == intent.Operation.ID && op.Result == nil && op.RetryAt.After(f.clock.Now())
	}) {
		t.Fatal("retired dispatch did not retain a retry ahead of the fixture clock")
	}
	cleanup.diagnose = func() {
		t.Logf("retired member %s is awaiting cleanup", address)
		logUnsettledLifecycleOperations(t, owner, address, f.s)
	}
	allowedAddress.Store(address)
	owner.WakeWorkflow()
	queued.Release()
	cleanup.await(t, f)
	ops, err = owner.Operations(address)
	fixturekit.Must(t, err)
	index := slices.IndexFunc(ops, func(op trace.OperationRecord) bool { return op.Operation.ID == intent.Operation.ID })
	if index < 0 || ops[index].Result == nil || ops[index].Result.Outcome != "cancelled" {
		logUnsettledLifecycleOperations(t, owner, address)
		t.Fatalf("retired member dispatch did not settle as cancelled: %+v", ops)
	}
	if drafts.Load() != 0 {
		t.Fatal("retired dispatch ran the shared delta turn")
	}
	after := currentChange(t, f.s, survivor, c.ID)
	if after.State == "applied" || len(after.Applied) != 0 || !intakefixture.EqualJSONTest(before.Before, after.Before) || !intakefixture.EqualJSONTest(before.Members, after.Members) {
		t.Fatal("retiring dispatch applied or repinned the shared decision")
	}
	cleanup.release()
	allowSurvivor.Store(true)
	lifecycleAwait(t, f, "retired delta controller cleanup", func() bool {
		a, found := intakefixture.ArchiveRecord(t, f.s.store, address)
		return found && !a.CleanedAt.IsZero()
	})
	if unavailable {
		lifecycleAwait(t, f, "survivor draft refused and settled", func() bool {
			c := currentChange(t, f.s, survivor, c.ID)
			return c.State == "failed" && c.FailureSettled
		})
		assertFrozenSliceRefusal(t, f, survivor, c.ID, address)
	} else {
		lifecycleAwait(t, f, "survivor delta presentation", func() bool {
			return currentChange(t, f.s, survivor, c.ID).State == "presented"
		})
	}
	pending := currentChange(t, f.s, survivor, c.ID)
	if drafts.Load() != 1 {
		t.Fatalf("survivor drafted %d times", drafts.Load())
	}
	f.stop(t)
	f.start(t)
	after = currentChange(t, f.s, survivor, c.ID)
	if !intakefixture.EqualJSONTest(pending, after) || drafts.Load() != 1 {
		t.Fatal("restart changed the pending decision or repeated its live contribution")
	}
}

func TestDesignProductionRetiredDeltaPreservesRunningSharedClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		unavailable        bool
		retireWhileRunning bool
	}{
		{name: "recorded-draft"},
		{name: "changed-retired-slice-refused", unavailable: true},
		{name: "running-at-retirement", retireWhileRunning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testRetiredDeltaPreservesRunningSharedClaim(t, tc.unavailable, tc.retireWhileRunning)
		})
	}
}

func testRetiredDeltaPreservesRunningSharedClaim(t *testing.T, unavailable, retireWhileRunning bool) {
	ctx := context.Background()
	queued := &intakefixture.ChangeRaceGate{Step: "change-delta-queued", Reached: make(chan struct{}), Released: make(chan struct{})}
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(queued.Release)
	t.Cleanup(cleanup.release)
	var allowedAddress atomic.Value
	allowedAddress.Store(config.WorkstreamID(""))
	var allowAll atomic.Bool
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, options: func(opts *Options) {
		limit := opts.Reconciliation.Limit
		opts.Reconciliation.Limit = func(op coreadapter.Operation) (func(), bool) {
			if op.Action == lifecycle.DeltaTurnAction {
				in, err := lifecycle.DecodeDeltaTurn(op)
				if err != nil || !allowAll.Load() && (in.Address != allowedAddress.Load().(config.WorkstreamID) || in.Tool != lifecycle.DeltaDraftTool) {
					return nil, false
				}
			}
			if limit != nil {
				return limit(op)
			}
			return nil, true
		}
	}, checkpoint: func(ctx context.Context, step string) error {
		if err := queued.Checkpoint(ctx, step); err != nil {
			return err
		}
		return cleanup.checkpoint(ctx, step)
	}}, "", nil)
	defer f.stop(t)
	running := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var drafts atomic.Int32
	var address config.WorkstreamID
	installRetirementDelta(t, f, &drafts)
	const key = "running-retired-dispatch"
	turn := lifecycle.DeltaTurnID(lifecycle.WorkgroupChange{ID: key}, intake.ArchitectAgent, lifecycle.DeltaDraftTool)
	f.engine.Mu.Lock()
	f.engine.Turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		drafts.Add(1)
		close(running)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return deliverRetirementDelta(ctx, req, tools, func(after *trace.ProjectSpec) {
			if unavailable {
				for i := range after.Slices {
					if after.Slices[i].Workstream == address {
						after.Slices[i].Spec += "\nAdditional handling preserves offsets.\n"
					}
				}
			}
		})
	}
	f.engine.Mu.Unlock()
	queued.Armed.Store(true)
	c, apiFault := f.s.requestChange(ctx, string(f.origin), lifecycle.ChangeRequest{Key: key, Kind: "amend", Scope: "workgroup", Change: "Revise the acknowledgment contract."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	queued.Await(t)
	var intent trace.OperationRecord
	var owner *trace.Repository
	for _, m := range resolutionMembers(f) {
		repo, err := f.s.repository(m.project)
		fixturekit.Must(t, err)
		ops, err := repo.Operations(m.stream)
		fixturekit.Must(t, err)
		for _, op := range ops {
			if op.Operation.Action == lifecycle.DeltaTurnAction {
				intent, address, owner = op, m.stream, repo
			}
		}
	}
	if address == "" || intent.Claim != nil || intent.Result != nil {
		t.Fatalf("delta was not queued before retirement: %+v", intent)
	}
	var in lifecycle.DeltaTurnInput
	fixturekit.Must(t, json.Unmarshal(intent.Operation.Input, &in))
	before := currentChange(t, f.s, f.origin, c.ID)
	awaitRunning := func() {
		t.Helper()
		select {
		case <-running:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("shared delta turn did not start")
		}
	}
	var active trace.QueuedTurn
	if retireWhileRunning {
		allowedAddress.Store(address)
		queued.Release()
		awaitRunning()
		wt, c, retired, closeAuthority, err := (deltaRunner{s: f.s}).load(ctx, in)
		fixturekit.Must(t, err)
		th, err := wt.Thread(c.Host, intake.ArchitectAgent)
		fixturekit.Must(t, err)
		index := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
		if retired || index < 0 {
			t.Fatal("retiring member did not start the shared turn")
		}
		active = th.Turns[index]
		if active.Claim == nil || active.Response != nil || !active.CompletedAt.IsZero() {
			t.Fatalf("shared turn was not claimed before retirement: %+v", active)
		}
		current, err := wt.CurrentTurnClaim(c.Host, active.Claim)
		fixturekit.Must(t, err)
		closeAuthority()
		if !current {
			t.Fatal("shared claim was not active before retirement")
		}
		ops, err := owner.Operations(address)
		fixturekit.Must(t, err)
		if !slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
			return op.Operation.ID == intent.Operation.ID && op.Claim != nil && op.Result == nil
		}) {
			t.Fatal("retiring member did not claim its dispatch")
		}
		allowedAddress.Store(config.WorkstreamID(""))
	}
	awaitMemberBuilt(t, f, owner, address)
	_, err := f.c.Abandon(ctx, address, "Owner cancelled the shared delta dispatch's member.")
	fixturekit.Must(t, err)
	_, err = f.c.Archive(ctx, address)
	fixturekit.Must(t, err)
	if !retireWhileRunning {
		survivor := f.stream1
		if survivor == address {
			survivor = f.stream2
		}
		allowedAddress.Store(survivor)
		queued.Release()
		awaitRunning()
	}
	observed, err := (deltaRunner{s: f.s}).Inspect(ctx, intent.Operation)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectUnknown || observed.Result != nil {
		t.Fatalf("running shared claim was settled: %+v", observed)
	}
	wt, after, retired, closeAuthority, err := (deltaRunner{s: f.s}).load(ctx, in)
	fixturekit.Must(t, err)
	th, err := wt.Thread(after.Host, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	closeAuthority()
	if !retired || !intakefixture.EqualJSONTest(before, after) || !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Claim != nil && q.Response == nil && q.CompletedAt.IsZero()
	}) {
		t.Fatal("retirement changed the shared journal or running claim")
	}
	if retireWhileRunning && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && intakefixture.EqualJSONTest(active, q)
	}) {
		t.Fatal("retirement changed the preexisting shared claim")
	}
	close(release)
	released = true
	lifecycleAwait(t, f, "shared delta response captured", func() bool {
		wt, c, _, closeAuthority, err := (deltaRunner{s: f.s}).load(ctx, in)
		fixturekit.Must(t, err)
		defer closeAuthority()
		th, err := wt.Thread(c.Host, intake.ArchitectAgent)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.TurnID == turn && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	wt, after, retired, closeAuthority, err = (deltaRunner{s: f.s}).load(ctx, in)
	fixturekit.Must(t, err)
	defer closeAuthority()
	th, err = wt.Thread(after.Host, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	if !retired || !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Response != nil && !q.CompletedAt.IsZero()
	}) {
		t.Fatal("running shared turn did not retain its captured response")
	}
	observed, err = (deltaRunner{s: f.s}).Inspect(ctx, intent.Operation)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectAbsent || observed.Result != nil {
		t.Fatalf("captured shared response was replaced by retirement: %+v", observed)
	}
	result, err := (deltaRunner{s: f.s}).Apply(ctx, intent.Operation)
	fixturekit.Must(t, err)
	outcome := "completed"
	if unavailable {
		outcome = "failed"
	}
	if result.Outcome != outcome {
		t.Fatalf("captured delta did not reconcile its ordinary outcome: %+v", result)
	}
	retained, err := wt.Thread(after.Host, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	if !intakefixture.EqualJSONTest(th, retained) {
		t.Fatal("reconciling retired controller changed the captured shared response")
	}
	_, latest, _, closeLatest, err := (deltaRunner{s: f.s}).load(ctx, in)
	fixturekit.Must(t, err)
	closeLatest()
	if unavailable {
		if latest.State != "failed" || !intakefixture.EqualJSONTest(before.Before, latest.Before) || !intakefixture.EqualJSONTest(before.After, latest.After) {
			t.Fatalf("refused draft changed the shared decision: %+v", latest)
		}
		survivor := f.stream1
		if survivor == address {
			survivor = f.stream2
		}
		assertFrozenSliceRefusal(t, f, survivor, c.ID, address)
	}
	if drafts.Load() != 1 {
		t.Fatalf("shared turn ran %d times", drafts.Load())
	}
	cleanup.diagnose = func() {
		t.Logf("retired member %s is awaiting cleanup", address)
		logUnsettledLifecycleOperations(t, owner, address, f.s)
	}
	allowAll.Store(true)
	owner.WakeWorkflow()
	cleanup.await(t, f)
	ops, err := owner.Operations(address)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
		return op.Operation.ID == intent.Operation.ID && op.Result != nil && op.Result.Outcome == outcome
	}) {
		t.Fatalf("captured retired dispatch did not settle: %+v", ops)
	}
	cleanup.release()
	lifecycleAwait(t, f, "running delta controller cleanup", func() bool {
		a, found := intakefixture.ArchiveRecord(t, f.s.store, address)
		return found && !a.CleanedAt.IsZero()
	})
	if drafts.Load() != 1 {
		t.Fatalf("shared turn ran %d times after cleanup", drafts.Load())
	}
}

func deliverRetirementDelta(ctx context.Context, req agent.Request, tools *mcp.ClientSession, mutations ...func(*trace.ProjectSpec)) (*agent.Result, error) {
	text, err := intakefixture.ReadTool(ctx, tools, "before.json")
	if err != nil {
		return nil, err
	}
	var after trace.ProjectSpec
	if err := json.Unmarshal([]byte(text), &after); err != nil {
		return nil, err
	}
	after.Behavior += " Acknowledgments retain their recorded offset."
	for _, mutate := range mutations {
		mutate(&after)
	}
	out, err := fixturekit.CallTool(ctx, tools, lifecycle.DeltaDraftTool, toolMap(after))
	if err == nil && strings.Contains(out, `"recorded":false`) {
		return intakefixture.QuestionResult(req, "session-retirement-delta", "Refused: "+out), nil
	}
	return intakefixture.QuestionResult(req, "session-retirement-delta", "The shared delta proposal is recorded."), err
}

func installRetirementDelta(t *testing.T, f *productionDesignFixture, drafts *atomic.Int32, mutations ...func(*trace.ProjectSpec)) {
	t.Helper()
	f.engine.Mu.Lock()
	defer f.engine.Mu.Unlock()
	previous := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		switch {
		case slices.Contains(names, lifecycle.DeltaDraftTool):
			drafts.Add(1)
			return deliverRetirementDelta(ctx, req, tools, mutations...)
		case slices.Contains(names, lifecycle.DeltaReviewTool):
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaReviewTool, map[string]any{"objections": []string{}, "note": "The existing member slices remain fixed."})
		case slices.Contains(names, lifecycle.DeltaReplyTool):
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaReplyTool, map[string]any{"reply": "The contract retains the recorded offset."})
		default:
			return previous(ctx, req, turn, tools)
		}
		return intakefixture.QuestionResult(req, "session-retirement-debate", "The delta contribution is recorded."), err
	}
}

func assertFrozenSliceRefusal(t *testing.T, f *productionDesignFixture, survivor config.WorkstreamID, change string, archived config.WorkstreamID) {
	t.Helper()
	project := f.project
	if survivor == f.stream2 {
		project = f.project2
	}
	repo, err := f.s.repository(project)
	fixturekit.Must(t, err)
	wt, host, _, closeAuthority, err := f.s.chiefRepository(context.Background(), repo, survivor)
	fixturekit.Must(t, err)
	defer closeAuthority()
	th, err := wt.Thread(host, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	turn := lifecycle.DeltaTurnID(lifecycle.WorkgroupChange{ID: change}, intake.ArchitectAgent, lifecycle.DeltaDraftTool)
	if !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Response != nil && strings.Contains(q.Response.Result.FinalResponse, "member "+string(archived)+" is archived and its slice is frozen")
	}) {
		t.Fatalf("draft changing archived member %s was not refused with its reason", archived)
	}
}

func awaitMemberBuilt(t *testing.T, f *productionDesignFixture, owner *trace.Repository, address config.WorkstreamID) {
	t.Helper()
	lifecycleAwait(t, f, "member build recorded", func() bool {
		ops, err := owner.Operations(address)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
			return op.Operation.Action == intake.BuildAction && op.Result != nil
		})
	})
}
