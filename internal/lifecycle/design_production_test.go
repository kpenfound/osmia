package lifecycle_test

import (
	context "context"
	errors "errors"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	api "github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	seal "github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func recordEngine(f *journey.ProductionDesignFixture, req agent.Request, handler string) {
	f.Dispatch.Mu.Lock()
	f.Dispatch.EngineName, f.Dispatch.Handler = req.Name, handler
	f.Dispatch.Mu.Unlock()
}

func awaitOwnerTurnRunning(f *journey.ProductionDesignFixture, t *testing.T, sent api.ConversationEntry, reached <-chan agent.Request) {
	t.Helper()
	wait, closeWait := journey.ServiceChanges(t, f.S)
	defer closeWait()
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	manager, host, _, closeManager, err := f.S.Lifecycle().ManagerRepository(context.Background(), repo, f.Origin)
	fixturekit.Must(t, err)
	defer closeManager()
	type memberTrace struct {
		repo   *trace.Repository
		stream config.WorkstreamID
	}
	var members []memberTrace
	for i, address := range []config.WorkstreamID{f.Origin, f.Stream1, f.Stream2} {
		project := f.Project
		if i == 2 {
			project = f.Project2
		}
		member, err := f.S.Repository(project)
		fixturekit.Must(t, err)
		members = append(members, memberTrace{member, address})
		target, routedHost, _, release, err := f.S.Lifecycle().ManagerRepository(context.Background(), member, address)
		fixturekit.Must(t, err)
		same := target == manager && routedHost == host
		t.Logf("owner address=%s project=%s authoritative-project=%s host=%s shared-handle=%v", address, project, target.Project(), routedHost, same)
		release()
		if !same {
			t.Fatal("continued owner address and members do not share the authoritative manager")
		}
	}
	diagnose := func() {
		t.Helper()
		th, err := manager.Thread(host, trace.Manager)
		if err != nil {
			t.Logf("owner turn=%s shared thread error=%v", sent.Turn, err)
			return
		}
		q, ready := lifecycle.NextReadyTurn(th)
		t.Logf("owner turn=%s now=%v shared-project=%s host=%s thread=%s status=%s active=%s parked=%v next=%s ready=%v", sent.Turn, f.Clock.Now(), manager.Project(), host, th.Identity.ThreadID, th.Status, th.Active, th.Parked(), q.Request.TurnID, ready)
		for _, turn := range th.Turns {
			if turn.Request.TurnID == sent.Turn {
				t.Logf("sent request=%s revision=%d project=%s host=%s actor=%+v accepted=%v prompt-matches=%v claim=%+v completed=%v", turn.Request.ID, turn.Request.Revision, turn.Request.Project, turn.Request.Workstream, turn.Request.Actor, turn.Request.At, turn.Request.Prompt == sent.Text, turn.Claim, turn.CompletedAt)
				if response := turn.Response; response != nil {
					t.Logf("sent turn=%s response=%s request-link=%s revision-link=%d failure=%s", sent.Turn, response.ID, response.RequestID, response.RequestRevision, response.Failure)
				}
			}
		}
		op, err := thread.TurnOperation(manager.Project(), "workgroup-manager-"+sent.Turn, thread.TurnInput{Workstream: host, Agent: trace.Manager, Turn: sent.Turn})
		fixturekit.Must(t, err)
		observed, err := (thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: manager}}).Inspect(context.Background(), op)
		t.Logf("owner dispatch operation=%s observation=%+v error=%v; shared manager dispatch applies directly without an operation retry timer", op.ID, observed, err)
		f.SharedDesign().LogTurns(t, manager, host)
	}
	diagnose()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		select {
		case req := <-reached:
			t.Logf("injected engine handler=fallback name=%s workspace=%s session=%s now=%v", req.Name, req.Workspace.Directory(), req.SessionDir, f.Clock.Now())
			if req.Name != sent.Turn || !strings.Contains(req.Prompt, sent.Text) {
				t.Fatalf("engine matched a different request: name=%s sent=%s", req.Name, sent.Turn)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			diagnose()
			f.Timeout(t, "continued owner turn engine entry")
		}
		now := f.Clock.Now()
		var next trace.OperationRecord
		var due memberTrace
		for _, member := range members {
			ops, err := member.repo.Operations(member.stream)
			fixturekit.Must(t, err)
			for _, op := range ops {
				if !op.Acknowledged && op.RetryAt.After(now) && (next.RetryAt.IsZero() || op.RetryAt.Before(next.RetryAt)) {
					next, due = op, member
				}
			}
		}
		if !next.RetryAt.IsZero() {
			t.Logf("owner turn=%s advances injected now=%v to retry=%v project=%s member=%s operation=%s action=%s observation=%+v", sent.Turn, now, next.RetryAt, due.repo.Project(), due.stream, next.Operation.ID, next.Operation.Action, next.Observation)
			f.Clock.Advance(next.RetryAt.Sub(now))
		}
		wait()
	}
}

func TestDesignProductionStartDraftRatifyAndRestart(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	f.CheckContinuation(t)
	if _, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision); err != nil {
		t.Fatal(err)
	}
	f.AwaitApproved(t)
	f.Wait(t, "members building", func() bool {
		for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			project := f.Project
			if i == 1 {
				project = f.Project2
			}
			repo, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			state, err := repo.Workflow(member, trace.FeatureSubject)
			fixturekit.Must(t, err)
			if state.Value != intake.BuildingState {
				return false
			}
		}
		return true
	})
	f.Stop(t)
	f.Start(t)
	f.CheckContinuation(t)
	f.AwaitApproved(t)
}

func TestDesignProductionStartAndApprovalRecoverAtBoundaries(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"design-member-allocated", "design-workgroup-prepared", "design-member-joined", "design-conversation-imported", "design-conversation-adopted", "group-decision-recorded", "group-member-decision-prepared", "seal-branch-created", "seal-recorded", "group-member-seal-prepared", "group-before-completion"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
			defer f.Stop(t)
			if step == "group-before-completion" {
				f.Stop(t)
				f.Start(t)
			}
			hook := &intakefixture.DesignInterruption{Step: step, Nth: 1, Reached: make(chan struct{})}
			f.Hook.Store(hook)
			f.StartOwner(t)
			var ratifyDone chan error
			if strings.HasPrefix(step, "group-") || strings.HasPrefix(step, "seal-") {
				packet := f.AwaitPacket(t)
				ratifyDone = make(chan error, 1)
				go func() {
					_, err := f.C.Ratify(context.Background(), f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
					ratifyDone <- err
				}()
			}
			select {
			case <-hook.Reached:
				t.Logf("reached %s design=%s members=%s/%s repos=%s/%s", step, f.Origin, f.Stream1, f.Stream2, f.Project, f.Project2)
			case <-time.After(fixturekit.DemoTimeout):
				f.Timeout(t, "interruption boundary "+step)
			}
			if hook.Lifetime != f.S.Lifetime || hook.Lifetime.Err() != nil {
				t.Fatal("interruption did not bind the running service lifetime")
			}
			if ratifyDone != nil {
				f.AssertBarrier(t)
			}
			f.Stop(t)
			t.Logf("stopped service at %s", step)
			if !errors.Is(hook.Lifetime.Err(), context.Canceled) {
				t.Fatal("service stop did not cancel the intercepted lifetime")
			}
			if ratifyDone != nil {
				<-ratifyDone
			}
			f.Hook.Store(nil)
			f.Start(t)
			t.Logf("restarted service after %s with injection cleared", step)
			packet := f.AwaitPacket(t)
			f.CheckContinuation(t)
			if ratifyDone == nil {
				_, err := f.C.Ratify(context.Background(), f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				fixturekit.Must(t, err)
			}
			f.AwaitApproved(t)
			f.CheckContinuation(t)
			t.Logf("completed both pinned seals after %s without repeating owner start or accepted ratification", step)
		})
	}
}

func TestDesignProductionRatificationRefusesInvalidPacketBeforeDecisions(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"cycle", "missing-slice"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
			defer f.Stop(t)
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			repo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			id, err := repo.CrossRepoWorkgroupOf(f.Stream1)
			fixturekit.Must(t, err)
			wt, _, err := journey.InspectForTest(f.S).ApprovalTrace(ctx, repo, id)
			fixturekit.Must(t, err)
			spec := *packet.ProjectSpec
			if invalid == "cycle" {
				spec.Graph = append(slices.Clone(spec.Graph), trace.Edge{From: f.Stream2, To: f.Stream1, Type: trace.EdgeContract})
			} else {
				spec.Slices = slices.DeleteFunc(slices.Clone(spec.Slices), func(slice trace.Slice) bool { return slice.Workstream == f.Stream2 })
			}
			writeCorruptedProjectSpec(t, wt, id, spec)
			fixturekit.Must(t, wt.Close())
			if _, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision); err == nil {
				t.Fatal("invalid packet received owner approval")
			}
			for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
				project := f.Project
				if i == 1 {
					project = f.Project2
				}
				r, err := f.S.Repository(project)
				fixturekit.Must(t, err)
				if _, found, err := r.GroupApprovalOf(member); err != nil || found {
					t.Fatalf("refusal recorded a group decision: %v %v", found, err)
				}
				if _, found, err := intake.LatestRatification(r, member); err != nil || found {
					t.Fatalf("refusal recorded a member decision: %v %v", found, err)
				}
				if _, _, found, err := seal.PreparedLatest(r, member); err != nil || found {
					t.Fatalf("refusal prepared a member seal: %v %v", found, err)
				}
			}
			f.CheckContinuation(t)
		})
	}
}

func TestDesignProductionExistingMemberApprovalsRecoverCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.StartOwner(t)
	f.AwaitPacket(t)
	for _, stream := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		owner, apiFault := f.S.ShedAction(string(stream), intake.InShedState, intake.SketchedState)
		if apiFault != nil {
			t.Fatal(apiFault)
		}
		if _, apiFault := journey.InspectForTest(f.S).RatifyMember(ctx, owner, owner.Pin); apiFault != nil {
			t.Fatal(apiFault)
		}
	}
	f.AwaitApproved(t)
	f.Stop(t)
	f.Start(t)
	f.AwaitApproved(t)
	f.CheckContinuation(t)
}

func TestDesignProductionApprovalRecoveryKeepsPinnedDrafts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	hook := &intakefixture.DesignInterruption{Step: "group-decision-recorded", Nth: 1, Reached: make(chan struct{})}
	f.Hook.Store(hook)
	returned := make(chan error, 1)
	go func() {
		_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		returned <- err
	}()
	select {
	case <-hook.Reached:
	case <-time.After(fixturekit.DemoTimeout):
		f.Timeout(t, "pinned owner decision")
	}
	f.AssertBarrier(t)
	for i, stream := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		project := f.Project
		if i == 1 {
			project = f.Project2
		}
		repo, err := f.S.Repository(project)
		fixturekit.Must(t, err)
		for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
			doc, err := trace.Get[trace.Document](repo, stream, id, 1)
			fixturekit.Must(t, err)
			doc.Revision, doc.At, doc.Cause = 2, f.Clock.Now(), "later-draft"
			if id == plan.SpecDocument {
				doc.Content += "\nAdditional unapproved draft text.\n"
			}
			fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
		}
	}
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	wt, group, err := journey.InspectForTest(f.S).ApprovalTrace(ctx, repo, id)
	fixturekit.Must(t, err)
	decision, _, found, err := trace.ReadGroupApproval(wt, group.Members[0])
	fixturekit.Must(t, err)
	if !found || decision.Complete || !intakefixture.EqualJSONTest(decision.ProjectSpec, *packet.ProjectSpec) {
		t.Fatal("owner decision did not pin the entire draft")
	}
	later := decision.ProjectSpec
	later.Behavior = "A later unapproved project draft."
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, wt, group, later, f.Clock.Now(), intake.OwnerActor, "later-draft"))
	fixturekit.Must(t, wt.Close())
	f.Stop(t)
	<-returned
	f.Hook.Store(nil)
	f.Start(t)
	f.AwaitApproved(t)
	repo, err = f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	completed, found, err := repo.GroupApprovalOf(f.Stream1)
	fixturekit.Must(t, err)
	decision.Complete = true
	if !found || !intakefixture.EqualJSONTest(decision, completed) {
		t.Fatal("recovery changed the immutable decision or its pins")
	}
	effective, _, found, err := repo.ProjectSpecOf(f.Stream1)
	fixturekit.Must(t, err)
	if !found || !intakefixture.EqualJSONTest(effective, decision.ProjectSpec) {
		t.Fatal("unapproved project draft became effective")
	}
	wt, group, err = journey.InspectForTest(f.S).ApprovalTrace(ctx, repo, id)
	fixturekit.Must(t, err)
	defer wt.Close()
	draft, found, err := trace.ProjectSpecFor(wt, group.ID)
	fixturekit.Must(t, err)
	if !found || draft.Behavior != later.Behavior {
		t.Fatal("recovery rewrote draft history")
	}
	f.CheckContinuation(t)
}

func TestDesignProductionContinuedStatusEscalationAndRuling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	f.Stop(t)
	f.Start(t)
	f.CheckContinuation(t)
	const ruling = "Keep the offset contract stable."
	const escalation = "May both repos change the offset contract?"
	var escalated, relayed atomic.Bool
	var unaffectedRuns atomic.Int32
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		recordEngine(f, req, "fallback")
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, status.ToolName) {
			return intakefixture.QuestionResult(req, "session-idle", "Working within the slice."), nil
		}
		if strings.Contains(req.Prompt, ruling) && relayed.CompareAndSwap(false, true) {
			_, err := fixturekit.CallTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.Stream1) + ":1", "text": ruling, "scope": "local"})
			return intakefixture.QuestionResult(req, "session-manager", "Ruling relayed."), err
		}
		_, err = fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Implement resumable uploads.", "note": "The offset contract remains agreed.", "agents": []string{}})
		if err != nil {
			return nil, err
		}
		if strings.Contains(req.Prompt, "affected-one") && strings.Contains(req.Prompt, "affected-two") && escalated.CompareAndSwap(false, true) {
			_, err = fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.Stream1) + ":1", string(f.Stream2) + ":1"}, "rephrasing": escalation, "blocked": "Both asking origins.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
		}
		return intakefixture.QuestionResult(req, "session-manager", "The service is following the agreed design."), err
	}
	f.Engine.Turns["affected-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		question := "affected-one: may the offset contract change?"
		if strings.Contains(req.SessionDir, "worker_second") {
			question = "affected-two: may the offset contract change?"
		}
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
		return intakefixture.QuestionResult(req, "session-worker", "Asked the manager."), err
	}
	f.Engine.Turns["unaffected-work"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		unaffectedRuns.Add(1)
		return intakefixture.QuestionResult(req, "session-unaffected", "Completed independent work."), nil
	}
	f.Engine.Turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ruling) {
			return nil, errors.New("origin resumed without its owner ruling")
		}
		return intakefixture.QuestionResult(req, "session-worker", "Resumed within the stable contract."), nil
	}
	f.Engine.Mu.Unlock()
	sent, err := f.C.Send(ctx, f.Stream2, "What is the current status of both repos?")
	fixturekit.Must(t, err)
	f.Owner, f.OwnerAddress = sent, f.Stream2
	memberRepo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	workgroup, err := memberRepo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	f.Wait(t, "owner status", func() bool {
		var feed api.WorkgroupFeedResponse
		fixturekit.Must(t, f.C.Do(ctx, "GET", api.Prefix+"/workgroups/"+string(workgroup)+"/feed", nil, &feed))
		return slices.ContainsFunc(feed.Entries, func(e api.FeedEntry) bool {
			return e.Kind == "status" && e.Status != nil && e.Status.Goal == "Implement resumable uploads."
		})
	})
	enqueue := func(p config.ProjectID, m config.WorkstreamID, id, turn string) {
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: id, Project: p, Workstream: m, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "workstream_created"}
		fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: intake.MasonRole, ThreadID: id}))
		h.Schema, h.ID = "osmia.trace.turn-request", "request_"+id
		_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: id, ThreadID: id, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are a member mason.", Prompt: "Perform scoped work."})
		fixturekit.Must(t, err)
	}
	enqueue(f.Project, f.Stream1, "worker_first", "affected-ask")
	enqueue(f.Project2, f.Stream2, "worker_second", "affected-ask")
	var inbox api.InboxResponse
	f.Wait(t, "member escalation", func() bool {
		got, err := f.C.Inbox(ctx)
		fixturekit.Must(t, err)
		got.Entries = slices.DeleteFunc(slices.Clone(got.Entries), func(entry api.InboxEntry) bool { return entry.Question != escalation })
		if len(got.Entries) > 1 {
			t.Fatalf("consolidated escalation duplicated: %+v", got)
		}
		if len(got.Entries) != 1 || len(got.Entries[0].Asked) != 2 {
			return false
		}
		for _, id := range []string{"worker_first", "worker_second"} {
			if !slices.ContainsFunc(got.Entries[0].Asked, func(q api.InboxQuestion) bool { return q.AskedBy == id }) {
				return false
			}
		}
		inbox = got
		return true
	})
	if len(inbox.Entries) != 1 {
		t.Fatalf("consolidated escalation duplicated: %+v", inbox)
	}
	entry := inbox.Entries[0]
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	inboxGroup, ok := trace.WorkgroupProjectID(entry.Project)
	if !ok || inboxGroup != group || entry.Number < 1 || entry.Batch == "" || entry.Workstream != f.Stream1 || entry.Kind != api.InboxEscalation || len(entry.Asked) != 2 {
		t.Fatalf("escalation lost workgroup/batch identity or affected origins: %+v; group=%s", entry, group)
	}
	for i, id := range []string{"worker_first", "worker_second"} {
		question := []string{"affected-one", "affected-two"}[i] + ": may the offset contract change?"
		if !slices.ContainsFunc(entry.Asked, func(q api.InboxQuestion) bool { return q.AskedBy == id && q.Question == question }) {
			t.Fatalf("consolidated entry omits origin %s: %+v", id, entry)
		}
	}
	enqueue(f.Project, f.Stream1, "worker_unaffected", "unaffected-work")
	f.Wait(t, "workers settled before escalation restart", func() bool {
		for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
			p, m := f.Project, f.Stream1
			if i == 1 {
				p, m = f.Project2, f.Stream2
			}
			repo, err := f.S.Repository(p)
			fixturekit.Must(t, err)
			worker, err := repo.Thread(m, id)
			fixturekit.Must(t, err)
			if len(worker.Turns) != 1 || worker.Turns[0].CompletedAt.IsZero() {
				return false
			}
			if i < 2 && !worker.Parked() {
				t.Fatalf("affected origin %s did not park on its question: %+v", id, worker)
			}
			if i == 2 && worker.Turns[0].Status() != "idle" {
				t.Fatalf("unaffected work did not complete successfully: %+v", worker)
			}
		}
		return unaffectedRuns.Load() == 1
	})
	before := map[string]trace.Thread{}
	asked := map[string]trace.QuestionState{}
	for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
		p, m := f.Project, f.Stream1
		if i == 1 {
			p, m = f.Project2, f.Stream2
		}
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		before[id], err = repo.Thread(m, id)
		fixturekit.Must(t, err)
		if i < 2 {
			qs, err := repo.Questions(m)
			fixturekit.Must(t, err)
			if len(qs) != 1 || qs[0].State != trace.QuestionEscalated || qs[0].Asked.Thread != id || qs[0].Asked.Turn != "affected-ask" || qs[0].Latest.Escalation == nil || qs[0].Latest.Escalation.Batch != entry.Batch || qs[0].Latest.Escalation.Inbox != entry.Number {
				t.Fatalf("origin %s lost its asking link or escalation identity: %+v", id, qs)
			}
			asked[id] = qs[0]
		}
	}
	f.Stop(t)
	f.Start(t)
	continued, err := f.C.Inbox(ctx)
	fixturekit.Must(t, err)
	continued.Entries = slices.DeleteFunc(slices.Clone(continued.Entries), func(entry api.InboxEntry) bool { return entry.Question != escalation })
	if len(continued.Entries) != 1 || !intakefixture.EqualJSONTest(entry, continued.Entries[0]) {
		t.Fatalf("restart lost escalation: %+v", continued)
	}
	for i, id := range []string{"worker_first", "worker_second"} {
		p, m := f.Project, f.Stream1
		if i == 1 {
			p, m = f.Project2, f.Stream2
		}
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		qs, err := repo.Questions(m)
		fixturekit.Must(t, err)
		if len(qs) != 1 || !intakefixture.EqualJSONTest(asked[id], qs[0]) {
			t.Fatalf("restart changed origin %s's durable asking link: %+v", id, qs)
		}
	}
	answer, err := f.C.Answer(ctx, continued.Entries[0].Number, ruling, continued.Entries[0].Project)
	fixturekit.Must(t, err)
	if answer.Number != entry.Number || answer.Batch != entry.Batch || answer.Ruling != ruling || len(answer.Questions) != 2 {
		t.Fatalf("owner ruling did not cover the selected consolidated question: %+v", answer)
	}
	f.Wait(t, "owner ruling resumes affected origins", func() bool {
		for i, id := range []string{"worker_first", "worker_second"} {
			p, m := f.Project, f.Stream1
			if i == 1 {
				p, m = f.Project2, f.Stream2
			}
			repo, err := f.S.Repository(p)
			fixturekit.Must(t, err)
			worker, err := repo.Thread(m, id)
			fixturekit.Must(t, err)
			if len(worker.Turns) != 2 || worker.Turns[1].CompletedAt.IsZero() {
				return false
			}
		}
		return true
	})
	for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
		p, m := f.Project, f.Stream1
		if i == 1 {
			p, m = f.Project2, f.Stream2
		}
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		worker, err := repo.Thread(m, id)
		fixturekit.Must(t, err)
		if i == 2 && !intakefixture.EqualJSONTest(before[id], worker) {
			t.Fatal("owner ruling resumed an unaffected origin")
		}
		if i < 2 && (!intakefixture.EqualJSONTest(before[id].Turns[0], worker.Turns[0]) || worker.Parked()) {
			t.Fatal("owner ruling changed the asking turn or failed to resume it")
		}
		if i < 2 {
			qs, err := repo.Questions(m)
			fixturekit.Must(t, err)
			if len(qs) != 1 || !intakefixture.EqualJSONTest(asked[id].Asked, qs[0].Asked) || qs[0].State != trace.QuestionAnswered || qs[0].Ruling == nil || qs[0].Ruling.Revision != 2 || qs[0].Ruling.OwnerResponse != ruling || qs[0].Ruling.ReturnedAnswer != ruling {
				t.Fatalf("origin %s did not retain its exact owner ruling and asking link: %+v", id, qs)
			}
			if len(worker.Turns) != 2 || worker.Turns[1].Request.TurnID != "answer_1" || worker.Turns[1].Response == nil || worker.Turns[1].Status() != "idle" || !strings.Contains(worker.Turns[1].Request.Prompt, ruling) {
				t.Fatalf("origin %s did not complete exactly one ruling continuation: %+v", id, worker)
			}
		}
	}
	f.CheckContinuation(t)
	if unaffectedRuns.Load() != 1 || !relayed.Load() {
		t.Fatalf("unaffected work or ruling relay failed: runs=%d relay=%v", unaffectedRuns.Load(), relayed.Load())
	}
}

func TestDesignProductionSingleAffectedRepoKeepsOrdinaryRatification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.Stream1 = f.Origin
	f.StartOwner(t)
	var packet api.PacketResponse
	f.Wait(t, "single-repo packet", func() bool {
		got, err := f.C.Packet(ctx, f.Origin)
		if err != nil {
			return false
		}
		packet = got
		return true
	})
	if packet.ProjectSpec != nil {
		t.Fatal("one affected repo introduced a project-spec gate")
	}
	out, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	if len(out.Members) != 0 || out.Sealing != "requested" {
		t.Fatalf("single-member owner contract changed: %+v", out)
	}
	f.Wait(t, "single-repo seal", func() bool {
		repo, err := f.S.Repository(f.Project)
		fixturekit.Must(t, err)
		_, _, found, err := seal.Latest(repo, f.Origin)
		fixturekit.Must(t, err)
		return found
	})
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	manager, err := repo.ManagerThread(f.Origin)
	fixturekit.Must(t, err)
	if !intakefixture.EqualJSONTest(manager.Identity, f.Before.Identity) || !intakefixture.EqualJSONTest(manager.Turns[:len(f.Before.Turns)], f.Before.Turns) {
		t.Fatal("single-member continuation changed")
	}
	groups, _, err := trace.Workgroups(repo)
	fixturekit.Must(t, err)
	group, found := trace.WorkgroupOf(groups, f.Origin)
	if !found || !slices.Equal(group.Members, []config.WorkstreamID{f.Origin}) {
		t.Fatal("single-member workgroup contract changed")
	}
}

func TestDesignProductionContinuedPendingTurnRecoversWithBoundedReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	reached := make(chan agent.Request, 1)
	var hit atomic.Bool
	const message = "Give the owner the current upload status after restart."
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		recordEngine(f, req, "fallback")
		if strings.Contains(req.Prompt, message) && hit.CompareAndSwap(false, true) {
			reached <- req
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return intakefixture.QuestionResult(req, "session-manager", "Following the approved contract."), nil
	}
	f.Engine.Mu.Unlock()
	sent, err := f.C.Send(ctx, f.Origin, message)
	fixturekit.Must(t, err)
	f.Owner, f.OwnerAddress = sent, f.Origin
	awaitOwnerTurnRunning(f, t, sent, reached)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	manager, host, _, close, err := f.S.Lifecycle().ManagerRepository(ctx, repo, f.Stream1)
	fixturekit.Must(t, err)
	before, err := manager.Thread(host, trace.Manager)
	close()
	fixturekit.Must(t, err)
	index := slices.IndexFunc(before.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == sent.Turn })
	if index < 0 || before.Active != sent.Turn || before.Turns[index].Claim == nil || before.Turns[index].Response != nil || !before.Turns[index].CompletedAt.IsZero() {
		t.Logf("continued manager status=%s active=%s sent=%s queue-index=%d", before.Status, before.Active, sent.Turn, index)
		t.Fatal("continued pending owner turn was not the durably claimed active request")
	}
	request := before.Turns[index].Request
	if request.Prompt != message || request.Actor != intake.OwnerActor || !request.At.Equal(sent.At) || request.Workstream != f.Origin || !intakefixture.EqualJSONTest(before.Identity, f.Before.Identity) {
		t.Fatalf("running owner request did not retain its accepted content or original conversation identity: header=%+v thread=%+v", request.Header, before.Identity)
	}
	f.Stop(t)
	var replayed atomic.Bool
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, message) {
			replayed.Store(true)
			_, err := fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Continue resumable uploads.", "note": "Restart retained the owner status request.", "agents": []string{}})
			return intakefixture.QuestionResult(req, "session-recovered", "Status continued after restart."), err
		}
		return intakefixture.QuestionResult(req, "session-manager", "Following the approved contract."), nil
	}
	f.Engine.Mu.Unlock()
	f.Start(t)
	f.Wait(t, "pending owner turn replay", func() bool { return replayed.Load() })
	repo, err = f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	manager, host, _, close, err = f.S.Lifecycle().ManagerRepository(ctx, repo, f.Stream1)
	fixturekit.Must(t, err)
	defer close()
	f.Wait(t, "recovery turn completion", func() bool {
		recovered, err := manager.Thread(host, trace.Manager)
		fixturekit.Must(t, err)
		return recovered.Active == "" && slices.ContainsFunc(recovered.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.Actor == intake.RecoveryActor && strings.Contains(q.Request.Prompt, message) && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	recovered, err := manager.Thread(host, trace.Manager)
	fixturekit.Must(t, err)
	q := recovered.Turns[index]
	if !intakefixture.EqualJSONTest(q.Request, request) || q.Response == nil || q.Response.RequestID != request.ID || q.Response.RequestRevision != request.Revision {
		t.Fatal("restart lost the pending request or response link")
	}
	followups := 0
	for _, turn := range recovered.Turns {
		if turn.Request.Actor == intake.RecoveryActor && strings.Contains(turn.Request.Prompt, message) {
			followups++
		}
	}
	if followups != 1 {
		t.Fatalf("recovery duplicated the continued turn: %d", followups)
	}
	if len(q.Attempts) > 2 {
		t.Fatalf("recovery exceeded the persisted attempt bound: %d", len(q.Attempts))
	}
	f.CheckContinuation(t)
}
