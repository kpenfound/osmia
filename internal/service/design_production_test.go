package service

import (
	context "context"
	errors "errors"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type designInterruption = intakefixture.DesignInterruption

type productionDesignFixture struct {
	*crossRepoRatifyFixture
	origin        config.WorkstreamID
	before        trace.Thread
	handed        trace.Document
	clock         *fixedClock
	hook          atomic.Pointer[designInterruption]
	repos         []productionDesignRepo
	owner         ConversationEntry
	ownerAddress  config.WorkstreamID
	dispatch      *productionChiefDispatch
	configuration productionDesignConfiguration
}

type productionChiefDispatch = intakefixture.ProductionChiefDispatch

type productionDesignRepo = intakefixture.ProductionDesignRepo

type productionDesignConfiguration struct {
	options    func(*Options)
	wait       func(*testing.T, *productionDesignFixture, string, func() bool)
	checkpoint func(context.Context, string) error
	handIn     bool
}

// newProductionDesignFixture prepares repos and injected engines only. The
// owner endpoint and production architect tools create all feature members,
// handed inputs and draft documents after the real design exchange.
func newProductionDesignFixture(t *testing.T) *productionDesignFixture {
	t.Helper()
	return newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
}

func newConfiguredProductionDesignFixture(t *testing.T, configuration productionDesignConfiguration) *productionDesignFixture {
	t.Helper()
	return intakefixture.NewProductionDesign(t, func(clock *fixedClock) intakefixture.ProductionDesignStart[*productionDesignFixture] {
		opts, clone, engine, sessions, _ := newArchitectOptions(t)
		intakefixture.ConfigureProductionDesign(t, opts.Config.Root)
		opts.Reconciliation.Now = clock.Now
		opts = Enforce(opts, Enforcement{Engine: engine, Hosts: opts.Architect.Hosts})
		opts.Librarian = nil
		if configuration.options != nil {
			configuration.options(&opts)
		}
		af := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions}
		f := &productionDesignFixture{crossRepoRatifyFixture: &crossRepoRatifyFixture{architectFixture: af}, clock: clock, dispatch: &productionChiefDispatch{}, configuration: configuration}
		return intakefixture.ProductionDesignStart[*productionDesignFixture]{
			Fixture: f, Scenario: f.sharedDesign(), Threads: opts.Threads, Hooks: &f.hook,
			Unwrap: func(turns coreadapter.Turns) *isolation.Turns {
				return turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
			},
			Bind: func(threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error), boundary func(context.Context, string) error) {
				af.opts.Threads, af.opts.boundary = threads, boundary
			},
			Start: f.start, Stop: f.stop, Running: func() bool { return f.s != nil },
		}
	}, configuration.handIn, configuration.wait != nil, configuration.checkpoint)
}

func (f *productionDesignFixture) start(t *testing.T) {
	t.Helper()
	f.architectFixture.start(t)
	f.checkRepos(t)
}

func (f *productionDesignFixture) checkRepos(t *testing.T) {
	t.Helper()
	f.sharedDesign().CheckRepos(t)
}

func (f *productionDesignFixture) wait(t *testing.T, phase string, done func() bool) {
	t.Helper()
	f.sharedDesign().Wait(t, phase, done)
}

func (f *productionDesignFixture) timeout(t *testing.T, phase string) {
	t.Helper()
	f.sharedDesign().Timeout(t, phase)
}

func (f *productionDesignFixture) recordEngine(req agent.Request, handler string) {
	f.dispatch.Mu.Lock()
	f.dispatch.EngineName, f.dispatch.Handler = req.Name, handler
	f.dispatch.Mu.Unlock()
}

func (f *productionDesignFixture) logTurns(t *testing.T, repo *trace.Repository, member config.WorkstreamID) {
	t.Helper()
	f.sharedDesign().LogTurns(t, repo, member)
}

// awaitOwnerTurnRunning waits for the injected engine, while the live service
// alone dispatches the durable request. Future retry deadlines advance the
// fixed clock only when an unfinished operation actually records one.
func (f *productionDesignFixture) awaitOwnerTurnRunning(t *testing.T, sent ConversationEntry, reached <-chan agent.Request) {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	repo, err := f.s.repository(f.project)
	must(t, err)
	chief, host, _, closeChief, err := f.s.chiefRepository(context.Background(), repo, f.origin)
	must(t, err)
	defer closeChief()
	type memberTrace struct {
		repo   *trace.Repository
		stream config.WorkstreamID
	}
	var members []memberTrace
	for i, address := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
		project := f.project
		if i == 2 {
			project = f.project2
		}
		member, err := f.s.repository(project)
		must(t, err)
		members = append(members, memberTrace{member, address})
		target, routedHost, _, release, err := f.s.chiefRepository(context.Background(), member, address)
		must(t, err)
		same := target == chief && routedHost == host
		t.Logf("owner address=%s project=%s authoritative-project=%s host=%s shared-handle=%v", address, project, target.Project(), routedHost, same)
		release()
		if !same {
			t.Fatal("continued owner address and members do not share the authoritative chief")
		}
	}
	diagnose := func() {
		t.Helper()
		th, err := chief.Thread(host, trace.ChiefOfStaff)
		if err != nil {
			t.Logf("owner turn=%s shared thread error=%v", sent.Turn, err)
			return
		}
		q, ready := nextReadyTurn(th)
		t.Logf("owner turn=%s now=%v shared-project=%s host=%s thread=%s status=%s active=%s parked=%v next=%s ready=%v", sent.Turn, f.clock.Now(), chief.Project(), host, th.Identity.ThreadID, th.Status, th.Active, th.Parked(), q.Request.TurnID, ready)
		for _, turn := range th.Turns {
			if turn.Request.TurnID == sent.Turn {
				t.Logf("sent request=%s revision=%d project=%s host=%s actor=%+v accepted=%v prompt-matches=%v claim=%+v completed=%v", turn.Request.ID, turn.Request.Revision, turn.Request.Project, turn.Request.Workstream, turn.Request.Actor, turn.Request.At, turn.Request.Prompt == sent.Text, turn.Claim, turn.CompletedAt)
				if response := turn.Response; response != nil {
					t.Logf("sent turn=%s response=%s request-link=%s revision-link=%d failure=%s", sent.Turn, response.ID, response.RequestID, response.RequestRevision, response.Failure)
				}
			}
		}
		op, err := thread.TurnOperation(chief.Project(), "workgroup-chief-"+sent.Turn, thread.TurnInput{Workstream: host, Agent: trace.ChiefOfStaff, Turn: sent.Turn})
		must(t, err)
		observed, err := (thread.Dispatcher{Runner: thread.Runner{Store: chief}}).Inspect(context.Background(), op)
		t.Logf("owner dispatch operation=%s observation=%+v error=%v; shared chief dispatch applies directly without an operation retry timer", op.ID, observed, err)
		f.logTurns(t, chief, host)
	}
	diagnose()
	deadline := time.Now().Add(demoTimeout)
	for {
		select {
		case req := <-reached:
			t.Logf("injected engine handler=fallback name=%s workspace=%s session=%s now=%v", req.Name, req.Workspace.Directory(), req.SessionDir, f.clock.Now())
			if req.Name != sent.Turn || !strings.Contains(req.Prompt, sent.Text) {
				t.Fatalf("engine matched a different request: name=%s sent=%s", req.Name, sent.Turn)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			diagnose()
			f.timeout(t, "continued owner turn engine entry")
		}
		now := f.clock.Now()
		var next trace.OperationRecord
		var due memberTrace
		for _, member := range members {
			ops, err := member.repo.Operations(member.stream)
			must(t, err)
			for _, op := range ops {
				if !op.Acknowledged && op.RetryAt.After(now) && (next.RetryAt.IsZero() || op.RetryAt.Before(next.RetryAt)) {
					next, due = op, member
				}
			}
		}
		if !next.RetryAt.IsZero() {
			t.Logf("owner turn=%s advances injected now=%v to retry=%v project=%s member=%s operation=%s action=%s observation=%+v", sent.Turn, now, next.RetryAt, due.repo.Project(), due.stream, next.Operation.ID, next.Operation.Action, next.Observation)
			f.clock.Advance(next.RetryAt.Sub(now))
		}
		wait()
	}
}

func (f *productionDesignFixture) startOwner(t *testing.T) {
	t.Helper()
	f.sharedDesign().StartOwner(t)
}

func (f *productionDesignFixture) awaitPacket(t *testing.T) PacketResponse {
	t.Helper()
	return f.sharedDesign().AwaitPacket(t)
}

func (f *productionDesignFixture) checkContinuation(t *testing.T) {
	t.Helper()
	f.sharedDesign().CheckContinuation(t)
}

var equalJSONTest = intakefixture.EqualJSONTest

func (f *productionDesignFixture) assertBarrier(t *testing.T) {
	t.Helper()
	f.sharedDesign().AssertBarrier(t)
}

func (f *productionDesignFixture) awaitApproved(t *testing.T) {
	t.Helper()
	f.sharedDesign().AwaitApproved(t)
}

func TestDesignProductionStartDraftRatifyAndRestart(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	f.checkContinuation(t)
	if _, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision); err != nil {
		t.Fatal(err)
	}
	f.awaitApproved(t)
	f.wait(t, "members building", func() bool {
		for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			state, err := repo.Workflow(member, trace.FeatureSubject)
			must(t, err)
			if state.Value != BuildingState {
				return false
			}
		}
		return true
	})
	f.stop(t)
	f.start(t)
	f.checkContinuation(t)
	f.awaitApproved(t)
}

func TestDesignProductionStartAndApprovalRecoverAtBoundaries(t *testing.T) {
	for _, step := range []string{"design-member-allocated", "design-workgroup-prepared", "design-member-joined", "design-conversation-imported", "design-conversation-adopted", "group-decision-recorded", "group-member-decision-prepared", "seal-branch-created", "seal-recorded", "group-member-seal-prepared", "group-before-completion"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f := newProductionDesignFixture(t)
			defer f.stop(t)
			if step == "group-before-completion" {
				// Arm this boundary on a new service to cover startup hook binding.
				f.stop(t)
				f.start(t)
			}
			hook := &designInterruption{Step: step, Nth: 1, Reached: make(chan struct{})}
			f.hook.Store(hook)
			f.startOwner(t)
			var ratifyDone chan error
			if strings.HasPrefix(step, "group-") || strings.HasPrefix(step, "seal-") {
				packet := f.awaitPacket(t)
				ratifyDone = make(chan error, 1)
				go func() {
					_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
					ratifyDone <- err
				}()
			}
			select {
			case <-hook.Reached:
				t.Logf("reached %s design=%s members=%s/%s repos=%s/%s", step, f.origin, f.stream1, f.stream2, f.project, f.project2)
			case <-time.After(demoTimeout):
				f.timeout(t, "interruption boundary "+step)
			}
			if hook.Lifetime != f.s.lifetime || hook.Lifetime.Err() != nil {
				t.Fatal("interruption did not bind the running service lifetime")
			}
			if ratifyDone != nil {
				f.assertBarrier(t)
			}
			f.stop(t)
			t.Logf("stopped service at %s", step)
			if !errors.Is(hook.Lifetime.Err(), context.Canceled) {
				t.Fatal("service stop did not cancel the intercepted lifetime")
			}
			if ratifyDone != nil {
				<-ratifyDone
			}
			f.hook.Store(nil)
			f.start(t)
			t.Logf("restarted service after %s with injection cleared", step)
			packet := f.awaitPacket(t)
			f.checkContinuation(t)
			if ratifyDone == nil {
				_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				must(t, err)
			}
			f.awaitApproved(t)
			f.checkContinuation(t)
			t.Logf("completed both pinned seals after %s without repeating owner start or accepted ratification", step)
		})
	}
}

func TestDesignProductionRatificationRefusesInvalidPacketBeforeDecisions(t *testing.T) {
	for _, invalid := range []string{"cycle", "missing-slice"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProductionDesignFixture(t)
			defer f.stop(t)
			f.startOwner(t)
			packet := f.awaitPacket(t)
			repo, err := f.s.repository(f.project)
			must(t, err)
			id, err := repo.CrossRepoWorkgroupOf(f.stream1)
			must(t, err)
			wt, _, err := f.s.approvalTrace(ctx, repo, id)
			must(t, err)
			spec := *packet.ProjectSpec
			if invalid == "cycle" {
				spec.Graph = append(slices.Clone(spec.Graph), trace.Edge{From: f.stream2, To: f.stream1, Type: trace.EdgeContract})
			} else {
				spec.Slices = slices.DeleteFunc(slices.Clone(spec.Slices), func(slice trace.Slice) bool { return slice.Workstream == f.stream2 })
			}
			// Corrupt only the finished production packet to exercise the owner
			// endpoint's validation independently of architect-tool validation.
			writeCorruptedProjectSpec(t, wt, id, spec)
			must(t, wt.Close())
			if _, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision); err == nil {
				t.Fatal("invalid packet received owner approval")
			}
			for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
				project := f.project
				if i == 1 {
					project = f.project2
				}
				r, err := f.s.repository(project)
				must(t, err)
				if _, found, err := r.GroupApprovalOf(member); err != nil || found {
					t.Fatalf("refusal recorded a group decision: %v %v", found, err)
				}
				if _, found, err := latestRatification(r, member); err != nil || found {
					t.Fatalf("refusal recorded a member decision: %v %v", found, err)
				}
				if _, _, found, err := seal.PreparedLatest(r, member); err != nil || found {
					t.Fatalf("refusal prepared a member seal: %v %v", found, err)
				}
			}
			f.checkContinuation(t)
		})
	}
}

func TestDesignProductionExistingMemberApprovalsRecoverCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	f.awaitPacket(t)
	// Existing installations can have individual owner decisions without
	// a group receipt. Exercise their reconciliation on production members.
	for _, stream := range []config.WorkstreamID{f.stream1, f.stream2} {
		owner, api := f.s.shedAction(string(stream), InShedState, SketchedState)
		if api != nil {
			t.Fatal(api)
		}
		if _, api := f.s.ratifyMember(ctx, owner, owner.Pin); api != nil {
			t.Fatal(api)
		}
	}
	f.awaitApproved(t)
	f.stop(t)
	f.start(t)
	f.awaitApproved(t)
	f.checkContinuation(t)
}

func TestDesignProductionApprovalRecoveryKeepsPinnedDrafts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	hook := &designInterruption{Step: "group-decision-recorded", Nth: 1, Reached: make(chan struct{})}
	f.hook.Store(hook)
	returned := make(chan error, 1)
	go func() {
		_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		returned <- err
	}()
	select {
	case <-hook.Reached:
	case <-time.After(demoTimeout):
		f.timeout(t, "pinned owner decision")
	}
	f.assertBarrier(t)
	for i, stream := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		repo, err := f.s.repository(project)
		must(t, err)
		for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
			doc, err := trace.Get[trace.Document](repo, stream, id, 1)
			must(t, err)
			doc.Revision, doc.At, doc.Cause = 2, f.clock.Now(), "later-draft"
			if id == plan.SpecDocument {
				doc.Content += "\nAdditional unapproved draft text.\n"
			}
			must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
		}
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	wt, group, err := f.s.approvalTrace(ctx, repo, id)
	must(t, err)
	decision, _, found, err := trace.ReadGroupApproval(wt, group.Members[0])
	must(t, err)
	if !found || decision.Complete || !equalJSONTest(decision.ProjectSpec, *packet.ProjectSpec) {
		t.Fatal("owner decision did not pin the entire draft")
	}
	later := decision.ProjectSpec
	later.Behavior = "A later unapproved project draft."
	must(t, trace.RecordProjectSpec(ctx, wt, group, later, f.clock.Now(), ownerActor, "later-draft"))
	must(t, wt.Close())
	f.stop(t)
	<-returned
	f.hook.Store(nil)
	f.start(t)
	f.awaitApproved(t)
	repo, err = f.s.repository(f.project)
	must(t, err)
	completed, found, err := repo.GroupApprovalOf(f.stream1)
	must(t, err)
	decision.Complete = true
	if !found || !equalJSONTest(decision, completed) {
		t.Fatal("recovery changed the immutable decision or its pins")
	}
	effective, _, found, err := repo.ProjectSpecOf(f.stream1)
	must(t, err)
	if !found || !equalJSONTest(effective, decision.ProjectSpec) {
		t.Fatal("unapproved project draft became effective")
	}
	wt, group, err = f.s.approvalTrace(ctx, repo, id)
	must(t, err)
	defer wt.Close()
	draft, found, err := trace.ProjectSpecFor(wt, group.ID)
	must(t, err)
	if !found || draft.Behavior != later.Behavior {
		t.Fatal("recovery rewrote draft history")
	}
	f.checkContinuation(t)
}

func TestDesignProductionSelectionRefusesInvalidReposBeforeAllocation(t *testing.T) {
	for _, kind := range []string{"empty", "duplicate", "outside"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newProductionDesignFixture(t)
			defer f.stop(t)
			selected := []string{}
			if kind == "duplicate" {
				selected = []string{string(f.project), string(f.project)}
			}
			if kind == "outside" {
				selected = []string{"p_000000000000000000000000000000ff"}
			}
			f.engine.Mu.Lock()
			f.engine.Turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if _, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": selected}); err == nil {
					return nil, errors.New("invalid selection accepted")
				}
				return questionResult(req, "session-refused", "Selection refused."), nil
			}
			f.engine.Mu.Unlock()
			f.startOwner(t)
			f.wait(t, "invalid selection completed", func() bool {
				repo, err := f.s.repository(f.project)
				must(t, err)
				state, err := repo.Workflow(f.origin, designStartSubject)
				must(t, err)
				return state.Value == "invalid"
			})
			for _, p := range []config.ProjectID{f.project, f.project2} {
				repo, err := f.s.repository(p)
				must(t, err)
				streams, err := repo.Workstreams()
				must(t, err)
				for _, m := range streams {
					if m != f.origin && m != librarianWorkstream(p) {
						t.Fatalf("invalid selection allocated a member %s", m)
					}
				}
			}
			repo, err := f.s.repository(f.project)
			must(t, err)
			chief, err := repo.ChiefOfStaffThread(f.origin)
			must(t, err)
			if !equalJSONTest(chief, f.before) {
				t.Fatal("invalid selection changed original conversation")
			}
			brief, err := trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
			must(t, err)
			if !equalJSONTest(brief, f.handed) {
				t.Fatal("invalid selection rewrote handed input")
			}
		})
	}
}

func TestDesignProductionContinuedStatusEscalationAndRuling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	f.stop(t)
	f.start(t)
	f.checkContinuation(t)
	const ruling = "Keep the offset contract stable."
	const escalation = "May both repos change the offset contract?"
	var escalated, relayed atomic.Bool
	var unaffectedRuns atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.recordEngine(req, "fallback")
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, status.ToolName) {
			return questionResult(req, "session-idle", "Working within the slice."), nil
		}
		if strings.Contains(req.Prompt, ruling) && relayed.CompareAndSwap(false, true) {
			_, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.stream1) + ":1", "text": ruling, "scope": "local"})
			return questionResult(req, "session-chief", "Ruling relayed."), err
		}
		_, err = callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Implement resumable uploads.", "note": "The offset contract remains agreed.", "agents": []string{}})
		if err != nil {
			return nil, err
		}
		if strings.Contains(req.Prompt, "affected-one") && strings.Contains(req.Prompt, "affected-two") && escalated.CompareAndSwap(false, true) {
			_, err = callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.stream1) + ":1", string(f.stream2) + ":1"}, "rephrasing": escalation, "blocked": "Both asking origins.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
		}
		return questionResult(req, "session-chief", "The service is following the agreed design."), err
	}
	f.engine.Turns["affected-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		question := "affected-one: may the offset contract change?"
		if strings.Contains(req.SessionDir, "worker_second") {
			question = "affected-two: may the offset contract change?"
		}
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
		return questionResult(req, "session-worker", "Asked the chief."), err
	}
	f.engine.Turns["unaffected-work"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		unaffectedRuns.Add(1)
		return questionResult(req, "session-unaffected", "Completed independent work."), nil
	}
	f.engine.Turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ruling) {
			return nil, errors.New("origin resumed without its owner ruling")
		}
		return questionResult(req, "session-worker", "Resumed within the stable contract."), nil
	}
	f.engine.Mu.Unlock()
	sent, err := f.c.Send(ctx, f.stream2, "What is the current status of both repos?")
	must(t, err)
	f.owner, f.ownerAddress = sent, f.stream2
	f.wait(t, "owner status", func() bool {
		feed, api := f.s.feedList(string(f.stream1))
		if api != nil {
			t.Fatal(api)
		}
		return slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool { return e.Kind == "status" && e.Status.Goal == "Implement resumable uploads." })
	})
	enqueue := func(p config.ProjectID, m config.WorkstreamID, id, turn string) {
		repo, err := f.s.repository(p)
		must(t, err)
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: id, Project: p, Workstream: m, At: f.clock.Now(), Actor: ownerActor, Cause: "workstream_created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: masonRole, ThreadID: id}))
		h.Schema, h.ID = "osmia.trace.turn-request", "request_"+id
		_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: id, ThreadID: id, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are a member mason.", Prompt: "Perform scoped work."})
		must(t, err)
	}
	enqueue(f.project, f.stream1, "worker_first", "affected-ask")
	enqueue(f.project2, f.stream2, "worker_second", "affected-ask")
	var inbox InboxResponse
	f.wait(t, "member escalation", func() bool {
		got, err := f.c.Inbox(ctx)
		must(t, err)
		got.Entries = slices.DeleteFunc(slices.Clone(got.Entries), func(entry InboxEntry) bool { return entry.Question != escalation })
		if len(got.Entries) > 1 {
			t.Fatalf("consolidated escalation duplicated: %+v", got)
		}
		if len(got.Entries) != 1 || len(got.Entries[0].Asked) != 2 {
			return false
		}
		for _, id := range []string{"worker_first", "worker_second"} {
			if !slices.ContainsFunc(got.Entries[0].Asked, func(q InboxQuestion) bool { return q.AskedBy == id }) {
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
	repo, err := f.s.repository(f.project)
	must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	inboxGroup, ok := trace.WorkgroupProjectID(entry.Project)
	if !ok || inboxGroup != group || entry.Number < 1 || entry.Batch == "" || entry.Workstream != f.stream1 || entry.Kind != InboxEscalation || len(entry.Asked) != 2 {
		t.Fatalf("escalation lost workgroup/batch identity or affected origins: %+v; group=%s", entry, group)
	}
	for i, id := range []string{"worker_first", "worker_second"} {
		question := []string{"affected-one", "affected-two"}[i] + ": may the offset contract change?"
		if !slices.ContainsFunc(entry.Asked, func(q InboxQuestion) bool { return q.AskedBy == id && q.Question == question }) {
			t.Fatalf("consolidated entry omits origin %s: %+v", id, entry)
		}
	}
	enqueue(f.project, f.stream1, "worker_unaffected", "unaffected-work")
	f.wait(t, "workers settled before escalation restart", func() bool {
		for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
			p, m := f.project, f.stream1
			if i == 1 {
				p, m = f.project2, f.stream2
			}
			repo, err := f.s.repository(p)
			must(t, err)
			worker, err := repo.Thread(m, id)
			must(t, err)
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
		p, m := f.project, f.stream1
		if i == 1 {
			p, m = f.project2, f.stream2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		before[id], err = repo.Thread(m, id)
		must(t, err)
		if i < 2 {
			qs, err := repo.Questions(m)
			must(t, err)
			if len(qs) != 1 || qs[0].State != trace.QuestionEscalated || qs[0].Asked.Thread != id || qs[0].Asked.Turn != "affected-ask" || qs[0].Latest.Escalation == nil || qs[0].Latest.Escalation.Batch != entry.Batch || qs[0].Latest.Escalation.Inbox != entry.Number {
				t.Fatalf("origin %s lost its asking link or escalation identity: %+v", id, qs)
			}
			asked[id] = qs[0]
		}
	}
	f.stop(t)
	f.start(t)
	continued, err := f.c.Inbox(ctx)
	must(t, err)
	continued.Entries = slices.DeleteFunc(slices.Clone(continued.Entries), func(entry InboxEntry) bool { return entry.Question != escalation })
	if len(continued.Entries) != 1 || !equalJSONTest(entry, continued.Entries[0]) {
		t.Fatalf("restart lost escalation: %+v", continued)
	}
	for i, id := range []string{"worker_first", "worker_second"} {
		p, m := f.project, f.stream1
		if i == 1 {
			p, m = f.project2, f.stream2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		qs, err := repo.Questions(m)
		must(t, err)
		if len(qs) != 1 || !equalJSONTest(asked[id], qs[0]) {
			t.Fatalf("restart changed origin %s's durable asking link: %+v", id, qs)
		}
	}
	answer, err := f.c.Answer(ctx, continued.Entries[0].Number, ruling, continued.Entries[0].Project)
	must(t, err)
	if answer.Number != entry.Number || answer.Batch != entry.Batch || answer.Ruling != ruling || len(answer.Questions) != 2 {
		t.Fatalf("owner ruling did not cover the selected consolidated question: %+v", answer)
	}
	f.wait(t, "owner ruling resumes affected origins", func() bool {
		for i, id := range []string{"worker_first", "worker_second"} {
			p, m := f.project, f.stream1
			if i == 1 {
				p, m = f.project2, f.stream2
			}
			repo, err := f.s.repository(p)
			must(t, err)
			worker, err := repo.Thread(m, id)
			must(t, err)
			if len(worker.Turns) != 2 || worker.Turns[1].CompletedAt.IsZero() {
				return false
			}
		}
		return true
	})
	for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
		p, m := f.project, f.stream1
		if i == 1 {
			p, m = f.project2, f.stream2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		worker, err := repo.Thread(m, id)
		must(t, err)
		if i == 2 && !equalJSONTest(before[id], worker) {
			t.Fatal("owner ruling resumed an unaffected origin")
		}
		if i < 2 && (!equalJSONTest(before[id].Turns[0], worker.Turns[0]) || worker.Parked()) {
			t.Fatal("owner ruling changed the asking turn or failed to resume it")
		}
		if i < 2 {
			qs, err := repo.Questions(m)
			must(t, err)
			if len(qs) != 1 || !equalJSONTest(asked[id].Asked, qs[0].Asked) || qs[0].State != trace.QuestionAnswered || qs[0].Ruling == nil || qs[0].Ruling.Revision != 2 || qs[0].Ruling.OwnerResponse != ruling || qs[0].Ruling.ReturnedAnswer != ruling {
				t.Fatalf("origin %s did not retain its exact owner ruling and asking link: %+v", id, qs)
			}
			if len(worker.Turns) != 2 || worker.Turns[1].Request.TurnID != "answer_1" || worker.Turns[1].Response == nil || worker.Turns[1].Status() != "idle" || !strings.Contains(worker.Turns[1].Request.Prompt, ruling) {
				t.Fatalf("origin %s did not complete exactly one ruling continuation: %+v", id, worker)
			}
		}
	}
	f.checkContinuation(t)
	if unaffectedRuns.Load() != 1 || !relayed.Load() {
		t.Fatalf("unaffected work or ruling relay failed: runs=%d relay=%v", unaffectedRuns.Load(), relayed.Load())
	}
}

func TestDesignProductionSingleAffectedRepoKeepsOrdinaryRatification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.stream1 = f.origin
	f.engine.Mu.Lock()
	f.engine.Turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": []string{string(f.project)}})
		return questionResult(req, "session-selection", "Only the server needs a change."), err
	}
	f.engine.Mu.Unlock()
	f.startOwner(t)
	var packet PacketResponse
	f.wait(t, "single-repo packet", func() bool {
		got, err := f.c.Packet(ctx, f.origin)
		if err != nil {
			return false
		}
		packet = got
		return true
	})
	if packet.ProjectSpec != nil {
		t.Fatal("one affected repo introduced a project-spec gate")
	}
	out, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	if len(out.Members) != 0 || out.Sealing != "requested" {
		t.Fatalf("single-member owner contract changed: %+v", out)
	}
	f.wait(t, "single-repo seal", func() bool {
		repo, err := f.s.repository(f.project)
		must(t, err)
		_, _, found, err := seal.Latest(repo, f.origin)
		must(t, err)
		return found
	})
	repo, err := f.s.repository(f.project)
	must(t, err)
	chief, err := repo.ChiefOfStaffThread(f.origin)
	must(t, err)
	if !equalJSONTest(chief.Identity, f.before.Identity) || !equalJSONTest(chief.Turns[:len(f.before.Turns)], f.before.Turns) {
		t.Fatal("single-member continuation changed")
	}
	groups, _, err := trace.Workgroups(repo)
	must(t, err)
	group, found := trace.WorkgroupOf(groups, f.origin)
	if !found || !slices.Equal(group.Members, []config.WorkstreamID{f.origin}) {
		t.Fatal("single-member workgroup contract changed")
	}
}

func TestDesignProductionContinuedPendingTurnRecoversWithBoundedReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	reached := make(chan agent.Request, 1)
	var hit atomic.Bool
	const message = "Give the owner the current upload status after restart."
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		f.recordEngine(req, "fallback")
		if strings.Contains(req.Prompt, message) && hit.CompareAndSwap(false, true) {
			reached <- req
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return questionResult(req, "session-chief", "Following the approved contract."), nil
	}
	f.engine.Mu.Unlock()
	sent, err := f.c.Send(ctx, f.origin, message)
	must(t, err)
	f.owner, f.ownerAddress = sent, f.origin
	f.awaitOwnerTurnRunning(t, sent, reached)
	repo, err := f.s.repository(f.project)
	must(t, err)
	chief, host, _, close, err := f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	before, err := chief.Thread(host, trace.ChiefOfStaff)
	close()
	must(t, err)
	index := slices.IndexFunc(before.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == sent.Turn })
	if index < 0 || before.Active != sent.Turn || before.Turns[index].Claim == nil || before.Turns[index].Response != nil || !before.Turns[index].CompletedAt.IsZero() {
		t.Logf("continued chief status=%s active=%s sent=%s queue-index=%d", before.Status, before.Active, sent.Turn, index)
		t.Fatal("continued pending owner turn was not the durably claimed active request")
	}
	request := before.Turns[index].Request
	if request.Prompt != message || request.Actor != ownerActor || !request.At.Equal(sent.At) || request.Workstream != f.origin || !equalJSONTest(before.Identity, f.before.Identity) {
		t.Fatalf("running owner request did not retain its accepted content or original conversation identity: header=%+v thread=%+v", request.Header, before.Identity)
	}
	f.stop(t)
	var replayed atomic.Bool
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, message) {
			replayed.Store(true)
			_, err := callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Continue resumable uploads.", "note": "Restart retained the owner status request.", "agents": []string{}})
			return questionResult(req, "session-recovered", "Status continued after restart."), err
		}
		return questionResult(req, "session-chief", "Following the approved contract."), nil
	}
	f.engine.Mu.Unlock()
	f.start(t)
	f.wait(t, "pending owner turn replay", func() bool { return replayed.Load() })
	repo, err = f.s.repository(f.project)
	must(t, err)
	chief, host, _, close, err = f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	defer close()
	f.wait(t, "recovery turn completion", func() bool {
		recovered, err := chief.Thread(host, trace.ChiefOfStaff)
		must(t, err)
		return recovered.Active == "" && slices.ContainsFunc(recovered.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.Actor == recoveryActor && strings.Contains(q.Request.Prompt, message) && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	recovered, err := chief.Thread(host, trace.ChiefOfStaff)
	must(t, err)
	q := recovered.Turns[index]
	if !equalJSONTest(q.Request, request) || q.Response == nil || q.Response.RequestID != request.ID || q.Response.RequestRevision != request.Revision {
		t.Fatal("restart lost the pending request or response link")
	}
	followups := 0
	for _, turn := range recovered.Turns {
		if turn.Request.Actor == recoveryActor && strings.Contains(turn.Request.Prompt, message) {
			followups++
		}
	}
	if followups != 1 {
		t.Fatalf("recovery duplicated the continued turn: %d", followups)
	}
	if len(q.Attempts) > 2 {
		t.Fatalf("recovery exceeded the persisted attempt bound: %d", len(q.Attempts))
	}
	f.checkContinuation(t)
}

func (f *productionDesignFixture) sharedDesign() *intakefixture.ProductionDesign {
	shared := &intakefixture.ProductionDesign{
		Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) },
		Scheduled: func() (*config.Config, []intakefixture.ScheduledProject) {
			cfg, active := f.s.runtimes()
			return cfg, intakefixture.ScheduledProjects(active, func(p *activeProject) intakefixture.ScheduledProject {
				interval := f.s.about(p.repository).Project.PullRequestPollInterval()
				return intakefixture.ScheduledProject{ID: p.id, Repository: p.repository, Interval: interval, NextPoll: f.s.polls.nextDue(p.id, interval, f.clock.Now())}
			})
		},
		LastEdgePoll: func(p config.ProjectID, w config.WorkstreamID) (time.Time, bool) {
			attempt, ok := f.s.polls.last(p, w, edgePoll)
			return attempt.at, ok
		},
		EdgeObservationAt: func(r *trace.Repository, w config.WorkstreamID) (int, time.Time, error) {
			_, rev, at, err := edgeObservationAt(r, w)
			return rev, at, err
		},

		Project: &f.project, Project2: &f.project2, Trace: &f.trace,
		Origin: &f.origin, Stream1: &f.stream1, Stream2: &f.stream2,
		Before: &f.before, Handed: &f.handed, Owner: &f.owner, OwnerAddress: &f.ownerAddress, Repos: &f.repos, ContextMode: func(p config.ProjectID) bundle.Mode { return f.s.Context().Mode(p) },
		Clone: f.clone, Engine: f.engine, Clock: f.clock, Dispatch: f.dispatch,
		Client:     func() intakefixture.ProductionDesignClient { return f.c },
		Config:     func() *config.Config { return f.s.current() },
		Repository: func(p config.ProjectID) (*trace.Repository, error) { return f.s.repository(p) },
		ChiefRepository: func(ctx context.Context, r *trace.Repository, w config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
			return f.s.chiefRepository(ctx, r, w)
		},
		Feed:  func(w string) (FeedResponse, *APIError) { return f.s.feedList(w) },
		Ready: func() bool { return f.s.ready.Load() }, Done: func() <-chan struct{} { return f.s.done },
		Lifetime: func() context.Context { return f.s.lifetime }, Error: func() error { return f.s.err }, Runs: f.runs,
		LogHook: func(t *testing.T) {
			if hook := f.hook.Load(); hook != nil {
				t.Logf("injection step=%s hits=%d target=%d", hook.Step, hook.Count.Load(), hook.Nth)
			}
		},
		WriteCharter: func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) },
		Upstream:     f.upstream, Script: f.script, ScriptDrafts: f.scriptDrafts, Workspaces: workspaces,
		ReadBuilding: func(r *trace.Repository, w config.WorkstreamID) (bool, error) {
			_, ok, err := (&masons{s: f.s, repository: r}).read(w)
			return ok, err
		},
		Holds: func(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
			return f.s.holds(p, w, r, c)
		},
		EdgeWaiting: edgeWaiting,
		Builder: func(r *trace.Repository) coreadapter.Reconciler {
			return &builder{S: f.s.intake().BuilderDependencies(), Repository: r}
		},
		Land: func(ctx context.Context, r *trace.Repository, m config.WorkstreamID) (coreadapter.OperationResult, error) {
			_, event := landIDs("accept", 1)
			return intakefixture.TryLanding(ctx, r, m, event, LandAction, landInput{Unit: "accept", Review: 1, Candidate: "candidate", Base: "base"}, (&foreman{masons: &masons{s: f.s, repository: r}}).Apply)
		},
		LeastWorkstream: leastWorkstream, NextReadyTurn: nextReadyTurn, LibrarianWorkstream: librarianWorkstream,
	}
	if f.configuration.wait != nil {
		shared.WaitMode = func(t *testing.T, phase string, done func() bool) { f.configuration.wait(t, f, phase, done) }
	}
	return shared
}
