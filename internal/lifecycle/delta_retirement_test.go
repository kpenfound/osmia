package lifecycle_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionQueuedDeltaRetiresBeforeExecution(t *testing.T) {
	t.Parallel()
	for _, unavailable := range []bool{false, true} {
		name := "unchanged-retired-slice"
		if unavailable {
			name = "changed-retired-slice-held"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testQueuedDeltaRetirement(t, unavailable)
		})
	}
}

func testQueuedDeltaRetirement(t *testing.T, unavailable bool) {
	ctx := context.Background()
	queued := &changeRaceGate{Step: "change-delta-queued", Reached: make(chan struct{}), Released: make(chan struct{})}
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(queued.Release)
	t.Cleanup(cleanup.release)
	// Admit the retired dispatch first. The survivor must not capture the
	// shared turn before cancellation of the unclaimed dispatch settles.
	var allowedAddress atomic.Value
	allowedAddress.Store(config.WorkstreamID(""))
	var allowSurvivor atomic.Bool
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, options: func(opts *Options) {
		limit := opts.Reconciliation.Limit
		opts.Reconciliation.Limit = func(op coreadapter.Operation) (func(), bool) {
			if op.Action == deltaTurnAction && !allowSurvivor.Load() {
				in, err := decodeDeltaTurn(op)
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
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "retired-dispatch", Kind: "amend", Scope: "workgroup", Change: "Revise the shared acknowledgment contract."})
	if api != nil {
		t.Fatal(api)
	}
	queued.Await(t)
	var owner *trace.Repository
	var intent trace.OperationRecord
	for _, m := range resolutionMembers(f) {
		repo, err := f.s.repository(m.project)
		must(t, err)
		ops, err := repo.Operations(m.stream)
		must(t, err)
		for _, op := range ops {
			if op.Operation.Action == deltaTurnAction {
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
	_, err := f.c.Abandon(ctx, address, "Owner cancelled the member before its queued delta ran.")
	must(t, err)
	_, err = f.c.Archive(ctx, address)
	must(t, err)
	// Inspection is read-only: retirement cancels the dispatch intent while
	// leaving every byte of the authoritative journal untouched.
	observed, err := (deltaRunner{s: f.s}).Inspect(ctx, intent.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectCompleted || observed.Result == nil || observed.Result.Outcome != "cancelled" {
		t.Fatalf("retired queued intent: %+v", observed)
	}
	survivor := f.stream1
	if survivor == address {
		survivor = f.stream2
	}
	if after := currentChange(t, f.s, survivor, c.ID); !equalJSONTest(before, after) {
		t.Fatal("retirement inspection changed the authoritative journal")
	}
	// Force a future retry while admission is fenced. The cleanup wait must
	// drive the fixture clock rather than depend on a retry happening by chance.
	attempt, _, err := owner.BeginOperation(ctx, address, intent.EventID, trace.WorkerOperation, trace.Actor{Kind: "service", ID: "retired-delta-retry"}, f.clock.Now)
	must(t, err)
	if attempt == nil {
		t.Fatal("could not force the retired dispatch retry")
	}
	retry := attempt.Action("retry", f.clock.Now())
	retry.Failure = "retired dispatch retry before cancellation inspection"
	retry.RetryAt = retry.At.Add(time.Second)
	err = attempt.Record(ctx, retry)
	attempt.Close()
	must(t, err)
	ops, err := owner.Operations(address)
	must(t, err)
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
	must(t, err)
	index := slices.IndexFunc(ops, func(op trace.OperationRecord) bool { return op.Operation.ID == intent.Operation.ID })
	if index < 0 || ops[index].Result == nil || ops[index].Result.Outcome != "cancelled" {
		logUnsettledLifecycleOperations(t, owner, address)
		t.Fatalf("retired member dispatch did not settle as cancelled: %+v", ops)
	}
	if drafts.Load() != 0 {
		t.Fatal("retired dispatch ran the shared delta turn")
	}
	after := currentChange(t, f.s, survivor, c.ID)
	if after.State == "applied" || len(after.Applied) != 0 || !equalJSONTest(before.Before, after.Before) || !equalJSONTest(before.Members, after.Members) {
		t.Fatal("retiring dispatch applied or repinned the shared decision")
	}
	cleanup.release()
	allowSurvivor.Store(true)
	lifecycleAwait(t, f, "retired delta controller cleanup", func() bool {
		a, found := archiveRecord(t, f.s.store, address)
		return found && !a.CleanedAt.IsZero()
	})
	if unavailable {
		lifecycleAwait(t, f, "survivor unavailable-history hold", func() bool {
			project := f.project
			if survivor == f.stream2 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			ops, err := repo.Operations(survivor)
			must(t, err)
			return slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
				return op.Operation.Action == deltaTurnAction && op.Result == nil && slices.ContainsFunc(op.History, func(a trace.OperationAction) bool {
					return a.Kind == "retry" && strings.Contains(a.Failure, string(address)) && strings.Contains(a.Failure, "unavailable validation history")
				})
			})
		})
		if got := currentChange(t, f.s, survivor, c.ID); !equalJSONTest(got, before) {
			t.Fatal("unavailable retired validation changed the pending journal")
		}
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
	if !equalJSONTest(pending, after) || drafts.Load() != 1 {
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
		{name: "unavailable-history", unavailable: true},
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
	queued := &changeRaceGate{Step: "change-delta-queued", Reached: make(chan struct{}), Released: make(chan struct{})}
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(queued.Release)
	t.Cleanup(cleanup.release)
	var allowedAddress atomic.Value
	allowedAddress.Store(config.WorkstreamID(""))
	var allowAll atomic.Bool
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, options: func(opts *Options) {
		limit := opts.Reconciliation.Limit
		opts.Reconciliation.Limit = func(op coreadapter.Operation) (func(), bool) {
			if op.Action == deltaTurnAction {
				in, err := decodeDeltaTurn(op)
				if err != nil || !allowAll.Load() && (in.Address != allowedAddress.Load().(config.WorkstreamID) || in.Tool != deltaDraftTool) {
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
	turn := deltaTurnID(WorkgroupChange{ID: key}, architectAgent, deltaDraftTool)
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
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: key, Kind: "amend", Scope: "workgroup", Change: "Revise the acknowledgment contract."})
	if api != nil {
		t.Fatal(api)
	}
	queued.Await(t)
	var intent trace.OperationRecord
	var owner *trace.Repository
	for _, m := range resolutionMembers(f) {
		repo, err := f.s.repository(m.project)
		must(t, err)
		ops, err := repo.Operations(m.stream)
		must(t, err)
		for _, op := range ops {
			if op.Operation.Action == deltaTurnAction {
				intent, address, owner = op, m.stream, repo
			}
		}
	}
	if address == "" || intent.Claim != nil || intent.Result != nil {
		t.Fatalf("delta was not queued before retirement: %+v", intent)
	}
	var in deltaTurnInput
	must(t, json.Unmarshal(intent.Operation.Input, &in))
	before := currentChange(t, f.s, f.origin, c.ID)
	awaitRunning := func() {
		t.Helper()
		select {
		case <-running:
		case <-time.After(demoTimeout):
			t.Fatal("shared delta turn did not start")
		}
	}
	var active trace.QueuedTurn
	if retireWhileRunning {
		// Only the member being retired may start this turn. Observe its
		// active claim before retirement, with the engine blocked throughout.
		allowedAddress.Store(address)
		queued.Release()
		awaitRunning()
		wt, c, retired, closeAuthority, err := (deltaRunner{s: f.s}).load(ctx, in)
		must(t, err)
		th, err := wt.Thread(c.Host, architectAgent)
		must(t, err)
		index := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
		if retired || index < 0 {
			t.Fatal("retiring member did not start the shared turn")
		}
		active = th.Turns[index]
		if active.Claim == nil || active.Response != nil || !active.CompletedAt.IsZero() {
			t.Fatalf("shared turn was not claimed before retirement: %+v", active)
		}
		current, err := wt.CurrentTurnClaim(c.Host, active.Claim)
		must(t, err)
		closeAuthority()
		if !current {
			t.Fatal("shared claim was not active before retirement")
		}
		ops, err := owner.Operations(address)
		must(t, err)
		if !slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
			return op.Operation.ID == intent.Operation.ID && op.Claim != nil && op.Result == nil
		}) {
			t.Fatal("retiring member did not claim its dispatch")
		}
		allowedAddress.Store(config.WorkstreamID(""))
	}
	_, err := f.c.Abandon(ctx, address, "Owner cancelled the shared delta dispatch's member.")
	must(t, err)
	_, err = f.c.Archive(ctx, address)
	must(t, err)
	if !retireWhileRunning {
		// Exercise the opposite order from the queued-cancellation journey:
		// the survivor runs while the retired dispatch stays fenced.
		survivor := f.stream1
		if survivor == address {
			survivor = f.stream2
		}
		allowedAddress.Store(survivor)
		queued.Release()
		awaitRunning()
	}
	observed, err := (deltaRunner{s: f.s}).Inspect(ctx, intent.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectUnknown || observed.Result != nil {
		t.Fatalf("running shared claim was settled: %+v", observed)
	}
	wt, after, retired, closeAuthority, err := (deltaRunner{s: f.s}).load(ctx, in)
	must(t, err)
	th, err := wt.Thread(after.Host, architectAgent)
	must(t, err)
	closeAuthority()
	if !retired || !equalJSONTest(before, after) || !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Claim != nil && q.Response == nil && q.CompletedAt.IsZero()
	}) {
		t.Fatal("retirement changed the shared journal or running claim")
	}
	if retireWhileRunning && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && equalJSONTest(active, q)
	}) {
		t.Fatal("retirement changed the preexisting shared claim")
	}
	close(release)
	released = true
	lifecycleAwait(t, f, "shared delta response captured", func() bool {
		wt, c, _, closeAuthority, err := (deltaRunner{s: f.s}).load(ctx, in)
		must(t, err)
		defer closeAuthority()
		th, err := wt.Thread(c.Host, architectAgent)
		must(t, err)
		return slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.TurnID == turn && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	wt, after, retired, closeAuthority, err = (deltaRunner{s: f.s}).load(ctx, in)
	must(t, err)
	defer closeAuthority()
	th, err = wt.Thread(after.Host, architectAgent)
	must(t, err)
	if !retired || !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Response != nil && !q.CompletedAt.IsZero()
	}) {
		t.Fatal("running shared turn did not retain its captured response")
	}
	observed, err = (deltaRunner{s: f.s}).Inspect(ctx, intent.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectAbsent || observed.Result != nil {
		t.Fatalf("captured shared response was replaced by retirement: %+v", observed)
	}
	result, err := (deltaRunner{s: f.s}).Apply(ctx, intent.Operation)
	must(t, err)
	if result.Outcome != "completed" {
		t.Fatalf("captured delta did not reconcile its ordinary outcome: %+v", result)
	}
	if unavailable && !strings.Contains(result.Evidence, "delta draft remains pending") {
		t.Fatalf("retired dispatch did not preserve the unavailable-history hold: %+v", result)
	}
	retained, err := wt.Thread(after.Host, architectAgent)
	must(t, err)
	if !equalJSONTest(th, retained) {
		t.Fatal("reconciling retired controller changed the captured shared response")
	}
	_, latest, _, closeLatest, err := (deltaRunner{s: f.s}).load(ctx, in)
	must(t, err)
	closeLatest()
	if unavailable && !equalJSONTest(before, latest) {
		t.Fatal("captured response with unavailable validation changed the shared decision")
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
	must(t, err)
	if !slices.ContainsFunc(ops, func(op trace.OperationRecord) bool {
		return op.Operation.ID == intent.Operation.ID && op.Result != nil && op.Result.Outcome == "completed"
	}) {
		t.Fatalf("captured retired dispatch did not settle: %+v", ops)
	}
	cleanup.release()
	lifecycleAwait(t, f, "running delta controller cleanup", func() bool {
		a, found := archiveRecord(t, f.s.store, address)
		return found && !a.CleanedAt.IsZero()
	})
	if drafts.Load() != 1 {
		t.Fatalf("shared turn ran %d times after cleanup", drafts.Load())
	}
}

func deliverRetirementDelta(ctx context.Context, req agent.Request, tools *mcp.ClientSession, mutations ...func(*trace.ProjectSpec)) (*agent.Result, error) {
	text, err := readTool(ctx, tools, "before.json")
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
	_, err = callTool(ctx, tools, deltaDraftTool, toolMap(after))
	if err != nil && strings.Contains(err.Error(), trace.ErrMemberRetired.Error()) {
		return questionResult(req, "session-retirement-delta", "The draft needs unavailable member history; keep the shared decision pending."), nil
	}
	return questionResult(req, "session-retirement-delta", "The shared delta proposal is recorded."), err
}

func installRetirementDelta(t *testing.T, f *productionDesignFixture, drafts *atomic.Int32, mutations ...func(*trace.ProjectSpec)) {
	t.Helper()
	f.engine.Mu.Lock()
	defer f.engine.Mu.Unlock()
	previous := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		switch {
		case slices.Contains(names, deltaDraftTool):
			drafts.Add(1)
			return deliverRetirementDelta(ctx, req, tools, mutations...)
		case slices.Contains(names, deltaReviewTool):
			_, err = callTool(ctx, tools, deltaReviewTool, map[string]any{"objections": []string{}, "note": "The existing member slices remain fixed."})
		case slices.Contains(names, deltaReplyTool):
			_, err = callTool(ctx, tools, deltaReplyTool, map[string]any{"reply": "The contract retains the recorded offset."})
		default:
			return previous(ctx, req, turn, tools)
		}
		return questionResult(req, "session-retirement-debate", "The delta contribution is recorded."), err
	}
}
