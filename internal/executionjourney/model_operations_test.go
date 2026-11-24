package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestArchitectDraftRunsBesidePassAndOtherWorkstreams(t *testing.T) {
	t.Parallel()
	var passes atomic.Int64
	f := newArchitectFixture(t, func(o *Options) { o.schedulePassed = func() { passes.Add(1) } })
	held, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if calls.Add(1) == 1 {
			close(held)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	defer f.stop(t)
	defer close(release)
	first := f.handIn(t, "held-draft", handedDesign)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("architect draft never started")
	}
	before := passes.Load()
	second := f.handIn(t, "independent-draft", handedDesign)
	f.await(t, second, sketched)
	f.await(t, second, func(_, _ string) bool { return InspectForTest(f.s).Pool().Slots(nil).Used[architectRole] == 1 })
	if passes.Load() <= before {
		t.Fatal("no scheduling pass completed beside the held draft")
	}
	if slots := InspectForTest(f.s).Pool().Slots(nil); slots.Used[architectRole] != 1 {
		t.Fatalf("held architect draft missing from capacity: %+v", slots)
	}
	if got := roleCapacity(t, f.s.capacityStatus(nil), architectRole); got.Used != 1 || got.Limit != nil || !got.Unbounded {
		t.Fatalf("architect capacity view: %+v", got)
	}
	if state, err := f.repository().Workflow(first, trace.FeatureSubject); err != nil || state.Value != HandedState {
		t.Fatalf("held draft state: %+v %v", state, err)
	}
}

func TestModelOperationsUseWorkflowOrderingBesideThePass(t *testing.T) {
	t.Parallel()
	for _, action := range []string{DraftAction, ReplyAction, RedraftAction, AmendmentDraftAction, AmendmentReplyAction, FinalReviewAction, deliveryReviewAction} {
		t.Run(action, func(t *testing.T) {
			op := coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: action}
			if !concurrentOperation(op) || operationClass(op) != trace.WorkflowOperation {
				t.Fatalf("operation admission: concurrent=%v class=%v", concurrentOperation(op), operationClass(op))
			}
		})
	}
}

func TestFinalReviewWaitsForClaimedCommitteeRoundWithoutBlockingPass(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, stream, repo, reviewer := newFinalFixture(t, "committee-capacity")
	assembleBoth(t, f, repo, reviewer, stream, map[string]string{"one.go": "package feature\n"}, map[string]string{"two.go": "package feature\n"})
	roundStream, err := config.NewWorkstreamID()
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, roundStream, f.s.now(), ownerActor))
	header := func(schema, id string) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, Revision: 1, ID: id, Project: repo.Project(), Workstream: roundStream, At: f.s.now(), Actor: ownerActor, Cause: "committee-round"}
	}
	agent := committeeAgent(1)
	must(t, repo.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", agent), Role: committeeRole, ThreadID: agent}))
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: header("osmia.trace.turn-request", "round-request"), AgentID: agent, ThreadID: agent, TurnID: roundTurnID(1, agent, 1), Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Read the plan"})
	must(t, err)
	claimed, err := repo.ClaimTurn(ctx, roundStream, agent, "round-token", t.TempDir(), f.s.now())
	must(t, err)
	must(t, InspectForTest(f.s).Pool().Join(repo))
	independent := coreadapter.Operation{ID: trace.OperationID(repo.Project(), roundStream, "independent"), Boundary: coreadapter.RepositoryBoundary, Action: "independent", Input: json.RawMessage(`{}`)}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: header("osmia.trace.transition", "independent"), Subject: "independent", To: "pending", Reason: "independent work remains eligible"}, Events: []trace.Event{{ID: "independent", Kind: "local-effect", Body: "independent work", Operation: &independent}}})
	must(t, err)
	f.s.setReviewChecks(passingChecks)
	var calls atomic.Int64
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		calls.Add(1)
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "The assembled feature satisfies the spec.", "findings": []any{}})
		return err
	})
	var passes int
	c, err := reconcile.New(repo, reconcile.Options{Worker: "committee-capacity-fixture", Now: f.s.now,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: reviewer, coreadapter.RepositoryBoundary: completedEffect{}},
		Schedule: func(context.Context) error { passes++; return nil }, Priority: stagePriority,
		Concurrent: concurrentOperation, Class: operationClass, Limit: InspectForTest(f.s).ModelOperationLimit(repo, nil)})
	must(t, err)
	defer func() { cancel(); c.Wait() }()
	for range 2 {
		must(t, c.Pass(ctx))
		must(t, c.Wait())
	}
	if review := finalOperations(t, repo, stream)[0]; review.Claim != nil || review.Result != nil || review.Acknowledged || calls.Load() != 0 {
		t.Fatalf("final review started with the committee pool full: %+v, model calls %d", review, calls.Load())
	}
	if passes != 2 || InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole] != 1 {
		t.Fatalf("passes %d, capacity %+v", passes, InspectForTest(f.s).Pool().Slots(nil))
	}
	ops, err := repo.Operations(roundStream)
	must(t, err)
	if len(ops) != 1 || !ops[0].Acknowledged {
		t.Fatalf("independent operation did not finish: %+v", ops)
	}
	completeClaimedTurn(t, f, repo, roundStream, agent, claimed, nil)
	must(t, c.Pass(ctx))
	must(t, c.Wait())
	if review := finalOperations(t, repo, stream)[0]; !review.Acknowledged || review.Result == nil || review.Result.Outcome != "succeeded" || calls.Load() != 1 {
		t.Fatalf("final review did not use the freed slot: %+v, model calls %d", review, calls.Load())
	}
}

func TestModelOperationsHoldForProviderUsageLimits(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t, func(o *Options) { o.Architect = nil })
	defer f.stop(t)
	for _, profile := range f.s.current().Profiles {
		must(t, f.s.store.SetProviderLimit(runtime.ProviderLimit{Backend: profile.Agent, Status: "blocked", SetAt: f.s.now()}))
	}
	stream := f.handIn(t, "provider-held-model-work", handedDesign)
	hold := InspectForTest(f.s).Holding(f.repository())
	for _, action := range []string{DraftAction, ReplyAction, RedraftAction, AmendmentDraftAction, AmendmentReplyAction, FinalReviewAction, deliveryReviewAction} {
		if op := (coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: action}); !hold(stream, op) {
			t.Errorf("provider usage limit did not hold %s before admission", action)
		}
	}
	if got := InspectForTest(f.s).Pool().Slots(nil).Used; got[architectRole] != 0 || got[committeeRole] != 0 {
		t.Fatalf("held model operations took capacity: %v", got)
	}
}

func TestFinalReviewRestartPreservesAnotherReviewsSettledJujutsuWork(t *testing.T) {
	t.Parallel()
	requireJJ(t)
	for _, point := range []string{"final-rebase-recorded", "final-rebase-moved"} {
		t.Run(point, func(t *testing.T) {
			f := newDebateFixture(t, 2, 1)
			var settled config.WorkstreamID
			hostService(t, f, func() config.WorkstreamID { return settled })
			base := f.upstream(t)
			first, repo := seedBuild(t, f, "interrupted-final", validPlan, config.WorkspacesJujutsu, base)
			a := &finalReviewer{s: f.s, repository: repo}
			_, one := assembleBoth(t, f, repo, a, first, map[string]string{"one.go": "package feature\n"}, map[string]string{"two.go": "package feature\n"})
			second := recordBuild(t, f, repo, "settled-final", validPlan, config.WorkspacesJujutsu, base)
			settled = second
			_, two := assembleBoth(t, f, repo, a, second, map[string]string{"three.go": "package feature\n"}, map[string]string{"four.go": "package feature\n"})
			advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
			f.opts.reviewChecks = passingChecks
			f.s.setReviewChecks(passingChecks)
			var modelCalls atomic.Int64
			f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				modelCalls.Add(1)
				_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "The assembled feature satisfies the spec.", "findings": []any{}})
				return err
			})
			f.s.boundary = func(name string) error {
				if name == point {
					return fmt.Errorf("interrupted after a settled local step")
				}
				return nil
			}
			if _, err := a.Apply(context.Background(), one); err == nil {
				t.Fatal("first review was not interrupted")
			}
			f.s.boundary = nil
			result, err := a.Apply(context.Background(), two)
			must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatalf("second review failed: %+v", result)
			}
			completed, found, err := latestFinalReport(repo, second)
			must(t, err)
			if !found {
				t.Fatal("second review recorded no report")
			}
			must(t, repo.Close())
			f.start(t)
			defer f.stop(t)
			for _, stream := range []config.WorkstreamID{first, second} {
				ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return finalOperations(t, f.repository(), stream) })
				if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
					t.Fatalf("recovered review: %+v", ops)
				}
				if reports := streamDocuments(t, f.repository(), stream, finalReportDocument); len(reports) != 1 {
					t.Fatalf("recorded %d final reports after restart", len(reports))
				}
				if rebases := streamDocuments(t, f.repository(), stream, finalRebaseDocument); len(rebases) != 1 {
					t.Fatalf("recorded %d rebases after restart", len(rebases))
				}
			}
			g, err := featureWorkspaces(f.s.about(f.repository()), f.repository()).Of(second)
			must(t, err)
			tip, exists, err := g.Branch(context.Background(), featureBranch(second))
			must(t, err)
			if !exists || tip != completed.Commit || modelCalls.Load() != 2 {
				t.Fatalf("restart changed settled work: tip %s, want %s; model calls %d", tip, completed.Commit, modelCalls.Load())
			}
		})
	}
}

func TestShedRedraftRestartReconcilesRecordedEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	f.script(roundTurnID(1, committeeAgent(1), 1), nil, nil)
	stream := f.handIn(t, "redraft-recovery", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	f.stop(t)
	architect := f.opts.Architect
	f.opts.Architect = nil
	f.start(t)
	_, err := f.c.ShedRedraft(ctx, stream, "Keep the same spec and plan, after checking them again.")
	must(t, err)
	f.stop(t)
	f.opts.Architect, f.s.options.Architect = architect, architect
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	d := &debate{S: f.s.intake().DebateDependencies(), Repository: repo}
	must(t, d.Pass(ctx))
	op := operationRecord(t, repo, stream, RedraftAction)
	release, admitted := InspectForTest(f.s).ModelOperationLimit(repo, nil)(op.Operation)
	if !admitted {
		t.Fatal("redraft did not get its architect slot")
	}
	defer release()
	attempt, _, err := repo.BeginOperation(ctx, stream, op.EventID, trace.WorkflowOperation, draftingActor, f.s.now)
	must(t, err)
	if attempt == nil {
		t.Fatal("redraft was not claimed")
	}
	defer attempt.Close()
	seen, err := (replier{Debate: d}).Inspect(ctx, op.Operation)
	must(t, err)
	observation := attempt.Action("observe", f.s.now())
	observation.Observation = &seen
	must(t, attempt.Record(ctx, observation))
	must(t, attempt.Record(ctx, attempt.Action("effect", f.s.now())))
	f.script((intake.RoundInput{Round: 1, Redraft: true}).TurnID(1), nil, nil)
	_, err = (replier{Debate: d}).Apply(ctx, op.Operation)
	must(t, err)
	before := operationRecord(t, repo, stream, RedraftAction)
	if before.Claim == nil || before.Result != nil || before.Acknowledged {
		t.Fatalf("redraft was not left in flight: %+v", before)
	}
	attempt.Close()
	release()
	must(t, repo.Close())
	f.start(t)
	records := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		return []trace.OperationRecord{operationRecord(t, f.repository(), stream, RedraftAction)}
	})
	if records[0].Result == nil || records[0].Result.Outcome != "succeeded" {
		t.Fatalf("recovered redraft: %+v", records[0])
	}
	if docs := f.documents(t, stream, shed.RedraftedDocumentID(1)); len(docs) != 1 {
		t.Fatalf("recorded %d redrafts", len(docs))
	}
	ran := map[string]int{}
	for _, turn := range f.runs() {
		ran[turn]++
	}
	if ran[(intake.RoundInput{Round: 1, Redraft: true}).TurnID(1)] != 1 || ran[(intake.RoundInput{Round: 1, Redraft: true}).TurnID(2)] != 0 {
		t.Fatalf("restart repeated the architect: %v", ran)
	}
}
