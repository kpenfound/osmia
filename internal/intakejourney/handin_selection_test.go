package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	"sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	issues "github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/lifecycle"
	questions "github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newHandInProductionFixture(t *testing.T) *productionDesignFixture {
	t.Helper()
	return newConfiguredProductionDesignFixture(t, productionDesignConfiguration{handIn: true, wait: lifecycleAwait})
}

// handInProduction hands the owner's ordered project list to the production API.
func handInProduction(t *testing.T, f *productionDesignFixture, req HandInRequest, projects ...config.ProjectID) HandInRequest {
	t.Helper()
	if len(projects) == 0 {
		projects = []config.ProjectID{f.project, f.project2}
	}
	if req.Project == "" {
		req.Projects = projects
	}
	r, err := f.s.repository(f.project)
	must(t, err)
	lock := intake.DesignLock(r, f.origin)
	lock.Lock()
	defer lock.Unlock()
	req.Project, req.Key = f.project, "production-handin"
	if req.Stdin == nil && req.Path == "" && req.URL == "" {
		text := productionBrief
		req.Stdin = &text
	}
	out, err := f.c.HandIn(context.Background(), req)
	must(t, err)
	if out.Workstream != f.origin {
		t.Fatalf("hand-in identity changed: %+v", out)
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	f.handed, err = trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
	must(t, err)
	f.before, err = repo.ChiefOfStaffThread(f.origin)
	must(t, err)
	return req
}

func checkHandInRetry(t *testing.T, f *productionDesignFixture, req HandInRequest) {
	t.Helper()
	out, err := f.c.HandIn(context.Background(), req)
	must(t, err)
	if out.Workstream != f.origin || out.Source != f.handed.Source {
		t.Fatalf("hand-in retry changed identity: %+v", out)
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	doc, err := trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
	must(t, err)
	if !equalJSONTest(doc, f.handed) {
		t.Fatal("hand-in retry rewrote the input")
	}
	docs, err := trace.Read[trace.Document](repo, f.origin)
	must(t, err)
	if count := len(slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != handedDocument })); count != 1 {
		t.Fatalf("handed revisions=%d", count)
	}
}

func TestHandInRecordedSelectionDraftsRatifiesAndRestarts(t *testing.T) {
	for _, form := range []string{"stdin", "file", "issue"} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			var req HandInRequest
			switch form {
			case "file":
				req.Path = filepath.Join(t.TempDir(), "request.md")
				must(t, os.WriteFile(req.Path, []byte(productionBrief), 0600))
			case "issue":
				req.URL = "https://github.com/owner/repo/issues/12"
				fake := f.opts.Issues.(*fakeIssues)
				fake.Mu.Lock()
				fake.Text = map[issues.Ref]string{{Owner: "owner", Repo: "repo", Number: 12}: productionBrief}
				fake.Mu.Unlock()
			}
			req = handInProduction(t, f, req)
			packet := f.awaitPacket(t)
			f.checkContinuation(t)
			repo, err := f.s.repository(f.project)
			must(t, err)
			selected, found, err := designSelectionOf(repo, f.origin)
			must(t, err)
			if !found || len(selected.Members) != 2 {
				t.Fatalf("selection not pinned: %+v", selected)
			}

			if form == "file" {
				must(t, os.Remove(req.Path))
			}
			checkHandInRetry(t, f, req)
			_, err = f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			must(t, err)
			f.awaitApproved(t)
			f.stop(t)
			f.start(t)
			checkHandInRetry(t, f, req)
			f.awaitApproved(t)
			f.checkContinuation(t)
			if form == "issue" {
				fake := f.opts.Issues.(*fakeIssues)
				fake.Mu.Lock()
				count := fake.Fetches
				fake.Mu.Unlock()
				if count != 1 {
					t.Fatalf("issue fetched %d times", count)
				}
			}
		})
	}
}

func TestHandInRecordedSelectionRecoversAllocationJoinAndSeal(t *testing.T) {
	for _, boundary := range []struct {
		step string
		nth  int32
	}{
		{"design-member-allocated", 1}, {"design-member-allocated", 2},
		{"design-workgroup-prepared", 1}, {"design-member-joined", 1}, {"design-member-joined", 2},
		{"design-conversation-imported", 1}, {"design-conversation-adopted", 1}, {"group-member-seal-prepared", 1},
	} {
		t.Run(fmt.Sprintf("%s-%d", boundary.step, boundary.nth), func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			hook := &designInterruption{Step: boundary.step, Nth: boundary.nth, Reached: make(chan struct{})}
			f.hook.Store(hook)
			req := handInProduction(t, f, HandInRequest{})
			var ratified chan error
			if strings.HasPrefix(boundary.step, "group-") {
				packet := f.awaitPacket(t)
				ratified = make(chan error, 1)
				go func() {
					_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
					ratified <- err
				}()
			}
			select {
			case <-hook.Reached:
			case <-time.After(demoTimeout):
				f.timeout(t, "hand-in interruption "+boundary.step)
			}
			if ratified != nil {
				f.assertBarrier(t)
			}
			repo, err := f.s.repository(f.project)
			must(t, err)
			pinned, found, err := designSelectionOf(repo, f.origin)
			must(t, err)
			if !found {
				t.Fatal("allocation began without pinned selection")
			}
			if strings.HasPrefix(boundary.step, "design-") {
				assertHandInJoiningCancellation(t, f, pinned.Members[0].Workstream)
			}
			f.stop(t)
			if ratified != nil {
				<-ratified
			}
			if strings.HasPrefix(boundary.step, "design-") {
				// Recovery must retain the guard while it finishes the same join.
				replay := &designInterruption{Step: boundary.step, Nth: 1, Reached: make(chan struct{})}
				f.hook.Store(replay)
				f.start(t)
				select {
				case <-replay.Reached:
				case <-time.After(demoTimeout):
					f.timeout(t, "recovered hand-in interruption "+boundary.step)
				}
				assertHandInJoiningCancellation(t, f, pinned.Members[0].Workstream)
				f.stop(t)
			}
			f.hook.Store(nil)
			f.start(t)
			packet := f.awaitPacket(t)
			repo, err = f.s.repository(f.project)
			must(t, err)
			recovered, found, err := designSelectionOf(repo, f.origin)
			must(t, err)
			if !found || !equalJSONTest(recovered, pinned) {
				t.Fatal("restart reselected members")
			}
			checkHandInRetry(t, f, req)
			f.checkContinuation(t)
			if ratified == nil {
				_, err = f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				must(t, err)
			}
			f.awaitApproved(t)
			f.checkContinuation(t)
		})
	}
}

func assertHandInJoiningCancellation(t *testing.T, f *productionDesignFixture, member config.WorkstreamID) {
	t.Helper()
	_, err := f.c.Abandon(context.Background(), member, "Drop a member during allocation")
	if !failed(err, Conflict) {
		t.Fatalf("member removed during allocation: %v", err)
	}
	controller := service.InspectForTest(f.s.core()).LifecycleController()
	for _, address := range []config.WorkstreamID{f.origin, member} {
		for _, scope := range []string{"workstream", "workgroup"} {
			_, api := controller.RequestChange(context.Background(), string(address), lifecycle.ChangeRequest{Key: "joining-" + scope, Kind: "cancel", Scope: scope, Change: "Cancel the temporary member grouping"})
			if api == nil || api.Code != Conflict || !strings.Contains(api.Message, "cancel the whole workgroup") {
				t.Fatalf("%s cancellation during allocation at %s: %v", scope, address, api)
			}
		}
	}
}

func TestHandInRecordedSingleSelectionKeepsOrdinaryPacket(t *testing.T) {
	for _, shorthand := range []bool{false, true} {
		for _, skip := range []bool{false, true} {
			name := "original"
			if shorthand {
				name = "shorthand"
			}
			if skip {
				name += "-skip"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newHandInProductionFixture(t)
				target := f.project
				f.stream1 = f.origin

				f.engine.Mu.Lock()
				for i := 1; i <= 2; i++ {
					f.engine.Turns[roundTurnID(1, committeeAgent(i), 1)] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
						return questionResult(req, "session-committee-"+req.Name, "The single-repo draft fits the charter."), nil
					}
				}
				f.engine.Mu.Unlock()
				request := HandInRequest{SkipDebate: skip}
				if shorthand {
					request.Project = target
				} else {
					request.Projects = []config.ProjectID{target}
				}
				req := handInProduction(t, f, request, target)
				var packet PacketResponse
				f.wait(t, "ordinary packet", func() bool {
					got, err := f.c.Packet(context.Background(), f.origin)
					if err != nil {
						return false
					}
					packet = got
					return true
				})
				if packet.ProjectSpec != nil || packet.Workstream != f.stream1 || packet.Project != target || packet.Packet.Skipped != skip {
					t.Fatalf("single selection changed packet: %+v", packet)
				}
				out, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				must(t, err)
				if len(out.Members) != 0 || out.Sealing != "requested" {
					t.Fatalf("single selection changed owner ratification: %+v", out)
				}
				f.wait(t, "ordinary seal", func() bool {
					repo, err := f.s.repository(target)
					must(t, err)
					_, _, found, err := seal.Latest(repo, f.stream1)
					must(t, err)
					return found
				})
				f.stop(t)
				f.start(t)
				checkHandInRetry(t, f, req)
				repo, err := f.s.repository(target)
				must(t, err)
				_, _, found, err := seal.Latest(repo, f.stream1)
				must(t, err)
				if !found {
					t.Fatal("restart lost ordinary seal")
				}
				if _, found, err := intake.DesignSelectionOf(repo, f.origin); err != nil || found {
					t.Fatalf("single-project hand-in recorded a selection: found=%v error=%v", found, err)
				}
				chief, host, _, closeChief, err := f.s.chiefRepository(context.Background(), repo, f.stream1)
				must(t, err)
				thread, err := chief.Thread(host, trace.ChiefOfStaff)
				closeChief()
				must(t, err)
				if !equalJSONTest(thread.Identity, f.before.Identity) {
					t.Fatal("single selection replaced the hand-in chief")
				}
				status, err := f.c.Status(context.Background(), f.stream1)
				must(t, err)
				if status.Project != target || status.Workgroup == nil || !slices.Equal(status.Workgroup.Members, []config.WorkstreamID{f.stream1}) || len(status.Workgroup.Edges) != 0 {
					t.Fatalf("single selection does not present one workstream in the selected repo: %+v", status)
				}
				source, err := f.c.Conversation(context.Background(), f.origin)
				must(t, err)
				member, err := f.c.Conversation(context.Background(), f.stream1)
				must(t, err)
				if !equalJSONTest(source.Entries, member.Entries) {
					t.Fatal("single selection split the conversation")
				}

			})
		}
	}
}

func TestHandInProductionMultipleReposPreservesSkippedDebate(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	req := handInProduction(t, f, HandInRequest{SkipDebate: true})
	var packet PacketResponse
	f.wait(t, "skipped multi-repo packet", func() bool {
		got, err := f.c.Packet(context.Background(), f.origin)
		if err != nil || got.ProjectSpec == nil {
			return false
		}
		packet = got
		return true
	})
	if !packet.Packet.Skipped {
		t.Fatal("allocation lost the owner's skip")
	}
	for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		repo, err := f.s.repository(project)
		must(t, err)
		threads, err := repo.Threads(m)
		must(t, err)
		if slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.Role == committeeRole }) {
			t.Fatal("committee ran despite hand-in skip")
		}
	}
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	checkHandInRetry(t, f, req)
	f.checkContinuation(t)
}

func TestHandInProductionSharedChiefResumesBothQuestionsAfterRestart(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	handInProduction(t, f, HandInRequest{})
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	f.engine.Mu.Lock()
	f.engine.Turns["handin-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "Should acknowledgments retain their offsets?"})
		return questionResult(req, "session-ask", "Asked the shared chief."), err
	}
	f.engine.Turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, "Keep the acknowledged offset.") {
			return nil, errors.New("answer went to the wrong asking turn")
		}
		return questionResult(req, "session-resumed", "Resumed within the contract."), nil
	}
	f.engine.Mu.Unlock()
	lifecycleWorker(t, f, f.project, f.stream1, "worker_first", "handin-ask", "Follow the server slice.")
	lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "handin-ask", "Follow the client slice.")
	originals := make([]trace.Question, 2)
	f.wait(t, "both open member questions", func() bool {
		ready := true
		for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			q, err := trace.Read[trace.Question](repo, m)
			must(t, err)
			if len(q) == 0 {
				ready = false
				continue
			}
			if len(q) != 1 {
				t.Fatalf("duplicated question before restart: %+v", q)
			}
			originals[i] = q[0]
		}
		return ready
	})
	f.stop(t)
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, questions.AnswerTool) {
			for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
				if _, err := callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(m) + ":1", "text": "Keep the acknowledged offset.", "citations": []string{"spec#1"}}); err != nil {
					return nil, err
				}
			}
		}
		return questionResult(req, "session-chief", "Both member questions were answered."), nil
	}
	f.engine.Mu.Unlock()
	f.start(t)
	f.wait(t, "both original askers resumed", func() bool {
		for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			worker := "worker_first"
			if i == 1 {
				project = f.project2
				worker = "worker_second"
			}
			repo, err := f.s.repository(project)
			must(t, err)
			th, err := repo.Thread(m, worker)
			must(t, err)
			if len(th.Turns) < 2 || th.Turns[1].CompletedAt.IsZero() {
				return false
			}
			if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != "answer_1" || th.Turns[1].Status() != "idle" {
				t.Fatalf("duplicated or failed continuation: %+v", th)
			}
			q, err := trace.Get[trace.Question](repo, m, "1", 1)
			must(t, err)
			if !equalJSONTest(q, originals[i]) {
				t.Fatal("restart changed original asking links")
			}
		}
		return true
	})
	f.checkContinuation(t)
}

func TestHandInProductionPreservesSameRepoBaseOnSelectedMember(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	parent := HandInWorkstream(f.project, "parent")
	f.engine.Mu.Lock()
	draft := f.engine.Turns["draft-1-1"]
	f.engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Workspace.Directory(), string(parent)) {
			return draft(ctx, req, verified, tools)
		}
		for path, content := range map[string]string{"spec.md": validSpec, "plan.json": validPlan} {
			if _, err := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); err != nil {
				return nil, err
			}
		}
		return questionResult(req, "session-parent-draft", "Parent drafted."), nil
	}
	f.engine.Mu.Unlock()
	text := "Implement the same-repo prerequisite."
	_, err := f.c.HandIn(context.Background(), HandInRequest{Project: f.project, Key: "parent", Stdin: &text, SkipDebate: true})
	must(t, err)
	var parentPacket PacketResponse
	f.wait(t, "parent packet", func() bool {
		got, err := f.c.Packet(context.Background(), parent)
		if err != nil {
			return false
		}
		parentPacket = got
		return true
	})
	_, err = f.c.Ratify(context.Background(), parent, parentPacket.Packet.Revision.Spec, parentPacket.Packet.Revision.Plan)
	must(t, err)
	f.wait(t, "parent seal", func() bool {
		repo, err := f.s.repository(f.project)
		must(t, err)
		_, _, ok, err := seal.Latest(repo, parent)
		must(t, err)
		return ok
	})
	req := handInProduction(t, f, HandInRequest{Base: parent})
	packet := f.awaitPacket(t)
	_, err = f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.wait(t, "dependent group seals", func() bool {
		for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			record, _, found, err := seal.Latest(repo, m)
			must(t, err)
			if !found {
				return false
			}
			base, err := repo.WorkstreamBase(m)
			must(t, err)
			if i == 0 {
				if base.Base != parent || base.Revision != 1 || record.Base.Workstream != parent || record.Base.Branch != featureBranch(parent) {
					t.Fatalf("same-repo base changed during allocation: %+v %+v", base, record.Base)
				}
			} else if base.Base != "" || record.Base.Workstream != "" {
				t.Fatalf("base leaked into another repo: %+v %+v", base, record.Base)
			}
		}
		return true
	})
	f.stop(t)
	f.start(t)
	checkHandInRetry(t, f, req)
	repo, err := f.s.repository(f.project)
	must(t, err)
	base, err := repo.WorkstreamBase(f.stream1)
	must(t, err)
	if base.Base != parent || base.Revision != 1 {
		t.Fatalf("restart changed the original base pin: %+v", base)
	}
}

func TestHandInOwnerProjectsValidationRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	before := map[config.ProjectID][]config.WorkstreamID{}
	for _, p := range []config.ProjectID{f.project, f.project2} {
		r, err := f.s.repository(p)
		must(t, err)
		before[p], err = r.Workstreams()
		must(t, err)
	}
	for _, projects := range [][]config.ProjectID{{}, {f.project, f.project}, {f.project, "p_0123456789abcdef0123456789abcdef"}} {
		text := productionBrief
		if api := handInError(t, f.c, HandInRequest{Projects: projects, Key: "invalid", Stdin: &text}); api.Code != Validation {
			t.Fatalf("projects %v: %+v", projects, api)
		}
	}
	writeCharter(t, f.c, f.project2, "# Empty charter\n")
	text := productionBrief
	if api := handInError(t, f.c, HandInRequest{Projects: []config.ProjectID{f.project, f.project2}, Key: "empty-charter", Stdin: &text}); api.Code != CharterEmpty {
		t.Fatalf("empty charter: %+v", api)
	}
	for p, want := range before {
		r, err := f.s.repository(p)
		must(t, err)
		got, err := r.Workstreams()
		must(t, err)
		if !slices.Equal(got, want) {
			t.Fatalf("refused hand-ins allocated work: %v -> %v", want, got)
		}
	}
}

func TestHandInOwnerProjectsKeepPausedMemberWaiting(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: f.project2}, Mode: "soft", Source: "owner"})
	handInProduction(t, f, HandInRequest{SkipDebate: true})
	f.wait(t, "active member drafts while paused member waits", func() bool {
		a, err := f.s.repository(f.project)
		must(t, err)
		state, err := a.Workflow(f.stream1, trace.FeatureSubject)
		if err != nil {
			return false
		}
		if state.Value != SketchedState && state.Value != InShedState {
			return false
		}
		b, err := f.s.repository(f.project2)
		must(t, err)
		state, err = b.Workflow(f.stream2, trace.FeatureSubject)
		must(t, err)
		if state.Value != HandedState {
			t.Fatalf("paused member advanced: %+v", state)
		}
		th, err := b.Threads(f.stream2)
		must(t, err)
		if slices.ContainsFunc(th, func(th trace.Thread) bool { return th.Identity.Role == "architect" && len(th.Turns) > 0 }) {
			t.Fatal("paused member ran an architect")
		}
		return true
	})
}

func TestHandInOwnerBaseAppliesOnlyToItsSelectedProject(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	b, err := f.s.repository(f.project2)
	must(t, err)
	base := HandInWorkstream(f.project2, "base")
	must(t, b.CreateWorkstreamOn(context.Background(), base, config.WorkspacesGit, f.clock.Now(), ownerActor))
	text := productionBrief
	if api := handInError(t, f.c, HandInRequest{Projects: []config.ProjectID{f.project}, Key: "base-refused", Stdin: &text, Base: base}); api.Code != Validation {
		t.Fatalf("unselected base: %+v", api)
	}
	handInProduction(t, f, HandInRequest{Base: base, SkipDebate: true})
	f.wait(t, "base pinned to its member", func() bool {
		pin, err := b.WorkstreamBase(f.stream2)
		if err != nil || pin.Base == "" {
			return false
		}
		if pin.Base != base || pin.Revision != 1 {
			t.Fatalf("other-project base: %+v", pin)
		}
		a, err := f.s.repository(f.project)
		must(t, err)
		pin, err = a.WorkstreamBase(f.stream1)
		must(t, err)
		if pin.Base != "" {
			t.Fatalf("base leaked into home member: %+v", pin)
		}
		return true
	})
}

func TestHandInOwnerProjectListCannotBeChanged(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	req := handInProduction(t, f, HandInRequest{})
	f.awaitPacket(t)
	for _, projects := range [][]config.ProjectID{{f.project}, {f.project, f.project2, f.project}} {
		req.Projects = projects
		api := handInError(t, f.c, req)
		if api.Code != Conflict && api.Code != Validation {
			t.Fatalf("changed list: %+v", api)
		}
	}
	for _, kind := range []string{"amend", "cancel"} {
		_, api := service.InspectForTest(f.s.core()).LifecycleController().RequestChange(context.Background(), string(f.origin), lifecycle.ChangeRequest{Key: "membership-" + kind, Kind: kind, Scope: "workstream", Workstream: f.stream2, Change: "Remove the second member"})
		if api == nil || api.Code != Conflict {
			t.Fatalf("%s before ratification: %v", kind, api)
		}
		if kind == "cancel" && !strings.Contains(api.Message, "cancel the whole workgroup") {
			t.Fatalf("cancellation refusal lacks the abandonment path: %v", api)
		}
	}
	r, err := f.s.repository(f.project)
	must(t, err)
	_, g, _, err := r.CrossRepoProjectSpec(config.WorkgroupID("g_" + strings.TrimPrefix(string(f.origin), "w_")))
	must(t, err)
	if !slices.Equal(g.Members, []config.WorkstreamID{f.origin, f.stream1, f.stream2}) {
		t.Fatalf("members changed: %+v", g)
	}
	_, err = f.c.Abandon(context.Background(), f.stream2, "Drop just this member")
	if !failed(err, Conflict) {
		t.Fatalf("abandon before ratification: %v", err)
	}
	_, err = f.c.MoveWorkstream(context.Background(), f.stream2, AbandonedState, "Drop this member through a move")
	if !failed(err, Conflict) {
		t.Fatalf("abandonment move before ratification: %v", err)
	}
	applyHandInCancellation(t, f, lifecycle.ChangeRequest{Key: "abandon-whole", Kind: "cancel", Scope: "workgroup", Change: "Abandon this selection and hand in again"})
	for _, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		_, _, repo, api := f.s.intake().ConversationTrace(string(member))
		if api != nil {
			t.Fatal(api)
		}
		state, err := repo.Workflow(member, trace.FeatureSubject)
		must(t, err)
		if state.Value != AbandonedState {
			t.Fatalf("whole-workgroup cancellation left member live: %s %+v", member, state)
		}
	}
}

func applyHandInCancellation(t *testing.T, f *productionDesignFixture, req lifecycle.ChangeRequest) {
	t.Helper()
	controller := service.InspectForTest(f.s.core()).LifecycleController()
	ctx := context.Background()
	change, api := controller.RequestChange(ctx, string(f.origin), req)
	if api != nil {
		t.Fatal(api)
	}
	wt, group, _, closeGroup, err := controller.ChangeContext(ctx, string(f.origin))
	must(t, err)
	defer closeGroup()
	f.wait(t, "cancellation impact presented", func() bool {
		must(t, controller.AdvanceChanges(ctx, wt, group))
		changes, err := lifecycle.ReadChanges(wt, group.Members[0])
		must(t, err)
		for _, recorded := range changes {
			if recorded.ID == change.ID {
				change = recorded
			}
		}
		return change.State == "presented"
	})
	change, api = controller.DecideChange(ctx, string(f.origin), change.ID, lifecycle.ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "close"})
	if api != nil {
		t.Fatal(api)
	}
	f.wait(t, "approved cancellation applied", func() bool {
		must(t, controller.AdvanceChanges(ctx, wt, group))
		changes, err := lifecycle.ReadChanges(wt, group.Members[0])
		must(t, err)
		for _, recorded := range changes {
			if recorded.ID == change.ID {
				change = recorded
			}
		}
		return change.State == "applied"
	})
}

func TestHandInOwnerMemberCancellationAfterRatification(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	handInProduction(t, f, HandInRequest{})
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	applyHandInCancellation(t, f, lifecycle.ChangeRequest{Key: "cancel-ratified-member", Kind: "cancel", Scope: "workstream", Workstream: f.stream2, Change: "Cancel just the client after ratification"})
	for _, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		_, _, repo, api := f.s.intake().ConversationTrace(string(member))
		if api != nil {
			t.Fatal(api)
		}
		state, err := repo.Workflow(member, trace.FeatureSubject)
		must(t, err)
		if (state.Value == AbandonedState) != (member == f.stream2) {
			t.Fatalf("post-ratification member cancellation: %s %+v", member, state)
		}
	}
	// A different member can be at its retirement snapshot boundary while
	// the owner abandons the survivor after shared ratification.
	repo, err := f.s.repository(f.project2)
	must(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- repo.WithMemberQuestionSnapshot(f.stream2, func(trace.MemberQuestionSnapshot) error {
			close(held)
			<-release
			return nil
		})
	}()
	defer func() {
		close(release)
		must(t, <-finished)
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("member retirement snapshot did not reach its boundary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := f.c.Abandon(ctx, f.stream1, "Cancel the ratified survivor while the other member retires")
	must(t, err)
	if out.State != AbandonedState {
		t.Fatalf("ratified survivor did not abandon: %+v", out)
	}
}

func TestArchitectNoChangeEscalatesWithSelectedMemberPresent(t *testing.T) {
	t.Parallel()
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{handIn: true})
	const question = "The selected client project needs no change. Ask the owner whether to abandon and hand in again."
	f.engine.Mu.Lock()
	draft := f.engine.Turns["draft-1-1"]
	f.engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.SystemPrompt, "Never add or drop a member") || !strings.Contains(req.SystemPrompt, "escalate that judgment to the owner") {
			return nil, errors.New("architect lacks fixed-member instructions")
		}
		if !strings.Contains(req.Workspace.Directory(), string(f.stream2)) {
			return draft(ctx, req, verified, tools)
		}
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
		return questionResult(req, "session-no-change", "Asked the chief to escalate."), err
	}
	original := f.engine.Turns["*"]
	var escalated atomic.Bool
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, questions.EscalateTool) && !escalated.Load() {
			r, err := f.s.repository(f.project2)
			if err != nil {
				return nil, err
			}
			qs, err := trace.Read[trace.Question](r, f.stream2)
			if err == nil && len(qs) > 0 {
				_, err = callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.stream2) + ":1"}, "rephrasing": question, "blocked": "Drafting the selected client member.", "options": []string{"Keep this selection", "Abandon and hand in again"}, "recommendation": "Keep the member; ask the owner to abandon and hand in again if the selection should change."})
				if err == nil {
					escalated.Store(true)
				}
				return questionResult(req, "session-chief-no-change", "Escalated the no-change judgment."), err
			}
		}
		return original(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()
	handInProduction(t, f, HandInRequest{})
	f.wait(t, "no-change judgment in the owner's inbox", func() bool {
		inbox, err := f.c.Inbox(context.Background())
		must(t, err)
		for _, e := range inbox.Entries {
			if e.Kind == "escalation" && strings.Contains(e.Question, "needs no change") {
				r, err := f.s.repository(f.project)
				must(t, err)
				id, err := r.CrossRepoWorkgroupOf(f.origin)
				must(t, err)
				_, g, _, err := r.CrossRepoProjectSpec(id)
				must(t, err)
				if !slices.Contains(g.Members, f.stream2) || len(g.Members) != 3 {
					t.Fatalf("no-change member was dropped: %+v", g)
				}
				return true
			}
		}
		return false
	})
}
