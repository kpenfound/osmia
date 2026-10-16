package service

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
	for _, unavailable := range []bool{false, true} {
		name := "unchanged-retired-slice"
		if unavailable {
			name = "changed-retired-slice-held"
		}
		t.Run(name, func(t *testing.T) { testQueuedDeltaRetirement(t, unavailable) })
	}
}

func testQueuedDeltaRetirement(t *testing.T, unavailable bool) {
	ctx := context.Background()
	queued := &changeRaceGate{step: "change-delta-queued", reached: make(chan struct{}), released: make(chan struct{})}
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(queued.release)
	t.Cleanup(cleanup.release)
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
		if err := queued.checkpoint(ctx, step); err != nil {
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
	queued.armed.Store(true)
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "retired-dispatch", Kind: "amend", Scope: "workgroup", Change: "Revise the shared acknowledgment contract."})
	if api != nil {
		t.Fatal(api)
	}
	queued.await(t)
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
	observed, err := (deltaRunner{f.s}).Inspect(ctx, intent.Operation)
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
	queued.release()
	cleanup.await(t)
	ops, err := owner.Operations(address)
	must(t, err)
	index := slices.IndexFunc(ops, func(op trace.OperationRecord) bool { return op.Operation.ID == intent.Operation.ID })
	if index < 0 || ops[index].Result == nil || ops[index].Result.Outcome != "cancelled" {
		logUnsettledLifecycleOperations(t, owner, address)
		t.Fatalf("retired member dispatch did not settle as cancelled: %+v", ops)
	}
	after := currentChange(t, f.s, survivor, c.ID)
	if after.State == "applied" || len(after.Applied) != 0 || !equalJSONTest(before.Before, after.Before) || !equalJSONTest(before.Members, after.Members) {
		t.Fatal("retiring dispatch applied or repinned the shared decision")
	}
	cleanup.release()
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
	ctx := context.Background()
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: cleanup.checkpoint}, "", nil)
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
	installRetirementDelta(t, f, &drafts)
	const key = "running-retired-dispatch"
	turn := deltaTurnID(WorkgroupChange{ID: key}, architectAgent, deltaDraftTool)
	f.engine.mu.Lock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		close(running)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return deliverRetirementDelta(ctx, req, tools)
	}
	f.engine.mu.Unlock()
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: key, Kind: "amend", Scope: "workgroup", Change: "Revise the acknowledgment contract."})
	if api != nil {
		t.Fatal(api)
	}
	select {
	case <-running:
	case <-time.After(demoTimeout):
		t.Fatal("shared delta turn did not start")
	}
	var intent trace.OperationRecord
	var address config.WorkstreamID
	for _, m := range resolutionMembers(f) {
		repo, err := f.s.repository(m.project)
		must(t, err)
		ops, err := repo.Operations(m.stream)
		must(t, err)
		for _, op := range ops {
			if op.Operation.Action == deltaTurnAction {
				intent, address = op, m.stream
			}
		}
	}
	if address == "" {
		t.Fatal("running delta has no member dispatch")
	}
	before := currentChange(t, f.s, f.origin, c.ID)
	_, err := f.c.Abandon(ctx, address, "Owner cancelled the delta controller while its shared turn ran.")
	must(t, err)
	_, err = f.c.Archive(ctx, address)
	must(t, err)
	observed, err := (deltaRunner{f.s}).Inspect(ctx, intent.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectUnknown || observed.Result != nil {
		t.Fatalf("running shared claim was settled: %+v", observed)
	}
	var in deltaTurnInput
	must(t, json.Unmarshal(intent.Operation.Input, &in))
	wt, after, retired, closeAuthority, err := (deltaRunner{f.s}).load(ctx, in)
	must(t, err)
	th, err := wt.Thread(after.Host, architectAgent)
	must(t, err)
	closeAuthority()
	if !retired || !equalJSONTest(before, after) || !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Claim != nil && q.Response == nil && q.CompletedAt.IsZero()
	}) {
		t.Fatal("retirement changed the shared journal or running claim")
	}
	close(release)
	released = true
	cleanup.await(t)
	wt, after, retired, closeAuthority, err = (deltaRunner{f.s}).load(ctx, in)
	must(t, err)
	defer closeAuthority()
	th, err = wt.Thread(after.Host, architectAgent)
	must(t, err)
	if !retired || !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn && q.Response != nil && !q.CompletedAt.IsZero()
	}) {
		t.Fatal("running shared turn did not retain its captured response")
	}
	observed, err = (deltaRunner{f.s}).Inspect(ctx, intent.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectAbsent || observed.Result != nil {
		t.Fatalf("captured shared response was replaced by retirement: %+v", observed)
	}
	result, err := (deltaRunner{f.s}).Apply(ctx, intent.Operation)
	must(t, err)
	if result.Outcome != "completed" {
		t.Fatalf("captured delta did not reconcile its ordinary outcome: %+v", result)
	}
	retained, err := wt.Thread(after.Host, architectAgent)
	must(t, err)
	if !equalJSONTest(th, retained) {
		t.Fatal("reconciling retired controller changed the captured shared response")
	}
	cleanup.release()
	lifecycleAwait(t, f, "running delta controller cleanup", func() bool {
		a, found := archiveRecord(t, f.s.store, address)
		return found && !a.CleanedAt.IsZero()
	})
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
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	previous := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
