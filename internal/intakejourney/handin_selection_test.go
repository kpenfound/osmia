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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

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

func newHandInProductionFixture(t *testing.T) *journey.ProductionDesignFixture {
	t.Helper()
	return journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{HandIn: true, Wait: journey.LifecycleAwait})
}

func handInProduction(t *testing.T, f *journey.ProductionDesignFixture, req api.HandInRequest, projects ...config.ProjectID) api.HandInRequest {
	t.Helper()
	if len(projects) == 0 {
		projects = []config.ProjectID{f.Project, f.Project2}
	}
	if req.Project == "" {
		req.Projects = projects
	}
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	lock := intake.DesignLock(r, f.Origin)
	lock.Lock()
	defer lock.Unlock()
	req.Project, req.Key = f.Project, "production-handin"
	if req.Stdin == nil && req.Path == "" && req.URL == "" {
		text := intakefixture.ProductionBrief
		req.Stdin = &text
	}
	out, err := f.C.HandIn(context.Background(), req)
	fixturekit.Must(t, err)
	if out.Workstream != f.Origin {
		t.Fatalf("hand-in identity changed: %+v", out)
	}
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	f.Handed, err = trace.Get[trace.Document](repo, f.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	f.Before, err = repo.ManagerThread(f.Origin)
	fixturekit.Must(t, err)
	return req
}

func checkHandInRetry(t *testing.T, f *journey.ProductionDesignFixture, req api.HandInRequest) {
	t.Helper()
	out, err := f.C.HandIn(context.Background(), req)
	fixturekit.Must(t, err)
	if out.Workstream != f.Origin || out.Source != f.Handed.Source {
		t.Fatalf("hand-in retry changed identity: %+v", out)
	}
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	doc, err := trace.Get[trace.Document](repo, f.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	if !intakefixture.EqualJSONTest(doc, f.Handed) {
		t.Fatal("hand-in retry rewrote the input")
	}
	docs, err := trace.Read[trace.Document](repo, f.Origin)
	fixturekit.Must(t, err)
	if count := len(slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != intake.HandedDocument })); count != 1 {
		t.Fatalf("handed revisions=%d", count)
	}
}

func TestHandInRecordedSelectionDraftsRatifiesAndRestarts(t *testing.T) {
	for _, form := range []string{"stdin", "file", "issue"} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			var req api.HandInRequest
			switch form {
			case "file":
				req.Path = filepath.Join(t.TempDir(), "request.md")
				fixturekit.Must(t, os.WriteFile(req.Path, []byte(intakefixture.ProductionBrief), 0600))
			case "issue":
				req.URL = "https://github.com/owner/repo/issues/12"
				fake := f.Opts.Issues.(*fixturekit.FakeIssues)
				fake.Mu.Lock()
				fake.Text = map[issues.Ref]string{{Owner: "owner", Repo: "repo", Number: 12}: intakefixture.ProductionBrief}
				fake.Mu.Unlock()
			}
			req = handInProduction(t, f, req)
			packet := f.AwaitPacket(t)
			f.CheckContinuation(t)
			repo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			selected, found, err := intake.DesignSelectionOf(repo, f.Origin)
			fixturekit.Must(t, err)
			if !found || len(selected.Members) != 2 {
				t.Fatalf("selection not pinned: %+v", selected)
			}

			if form == "file" {
				fixturekit.Must(t, os.Remove(req.Path))
			}
			checkHandInRetry(t, f, req)
			_, err = f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			f.AwaitApproved(t)
			f.Stop(t)
			f.Start(t)
			checkHandInRetry(t, f, req)
			f.AwaitApproved(t)
			f.CheckContinuation(t)
			if form == "issue" {
				fake := f.Opts.Issues.(*fixturekit.FakeIssues)
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
			hook := &intakefixture.DesignInterruption{Step: boundary.step, Nth: boundary.nth, Reached: make(chan struct{})}
			f.Hook.Store(hook)
			req := handInProduction(t, f, api.HandInRequest{})
			var ratified chan error
			if strings.HasPrefix(boundary.step, "group-") {
				packet := f.AwaitPacket(t)
				ratified = make(chan error, 1)
				go func() {
					_, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
					ratified <- err
				}()
			}
			select {
			case <-hook.Reached:
			case <-time.After(fixturekit.DemoTimeout):
				f.Timeout(t, "hand-in interruption "+boundary.step)
			}
			if ratified != nil {
				f.AssertBarrier(t)
			}
			repo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			pinned, found, err := intake.DesignSelectionOf(repo, f.Origin)
			fixturekit.Must(t, err)
			if !found {
				t.Fatal("allocation began without pinned selection")
			}
			if strings.HasPrefix(boundary.step, "design-") {
				assertHandInJoiningCancellation(t, f, pinned.Members[0].Workstream)
			}
			f.Stop(t)
			if ratified != nil {
				<-ratified
			}
			if strings.HasPrefix(boundary.step, "design-") {
				replay := &intakefixture.DesignInterruption{Step: boundary.step, Nth: 1, Reached: make(chan struct{})}
				f.Hook.Store(replay)
				f.Start(t)
				select {
				case <-replay.Reached:
				case <-time.After(fixturekit.DemoTimeout):
					f.Timeout(t, "recovered hand-in interruption "+boundary.step)
				}
				assertHandInJoiningCancellation(t, f, pinned.Members[0].Workstream)
				f.Stop(t)
			}
			f.Hook.Store(nil)
			f.Start(t)
			packet := f.AwaitPacket(t)
			repo, err = f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			recovered, found, err := intake.DesignSelectionOf(repo, f.Origin)
			fixturekit.Must(t, err)
			if !found || !intakefixture.EqualJSONTest(recovered, pinned) {
				t.Fatal("restart reselected members")
			}
			checkHandInRetry(t, f, req)
			f.CheckContinuation(t)
			if ratified == nil {
				_, err = f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				fixturekit.Must(t, err)
			}
			f.AwaitApproved(t)
			f.CheckContinuation(t)
		})
	}
}

func assertHandInJoiningCancellation(t *testing.T, f *journey.ProductionDesignFixture, member config.WorkstreamID) {
	t.Helper()
	_, err := f.C.Abandon(context.Background(), member, "Drop a member during allocation")
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("member removed during allocation: %v", err)
	}
	controller := service.InspectForTest(f.S.Core()).LifecycleController()
	for _, address := range []config.WorkstreamID{f.Origin, member} {
		for _, scope := range []string{"workstream", "workgroup"} {
			_, apiErr := controller.RequestChange(context.Background(), string(address), lifecycle.ChangeRequest{Key: "joining-" + scope, Kind: "cancel", Scope: scope, Change: "Cancel the temporary member grouping"})
			if apiErr == nil || apiErr.Code != api.Conflict || !strings.Contains(apiErr.Message, "cancel the whole workgroup") {
				t.Fatalf("%s cancellation during allocation at %s: %v", scope, address, apiErr)
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
				target := f.Project
				f.Stream1 = f.Origin

				f.Engine.Mu.Lock()
				for i := 1; i <= 2; i++ {
					f.Engine.Turns[intake.RoundTurnID(1, intake.CommitteeAgent(i), 1)] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
						return intakefixture.QuestionResult(req, "session-committee-"+req.Name, "The single-repo draft fits the charter."), nil
					}
				}
				f.Engine.Mu.Unlock()
				request := api.HandInRequest{SkipDebate: skip}
				if shorthand {
					request.Project = target
				} else {
					request.Projects = []config.ProjectID{target}
				}
				req := handInProduction(t, f, request, target)
				var packet api.PacketResponse
				f.Wait(t, "ordinary packet", func() bool {
					got, err := f.C.Packet(context.Background(), f.Origin)
					if err != nil {
						return false
					}
					packet = got
					return true
				})
				if packet.ProjectSpec != nil || packet.Workstream != f.Stream1 || packet.Project != target || packet.Packet.Skipped != skip {
					t.Fatalf("single selection changed packet: %+v", packet)
				}
				out, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				fixturekit.Must(t, err)
				if len(out.Members) != 0 || out.Sealing != "requested" {
					t.Fatalf("single selection changed owner ratification: %+v", out)
				}
				f.Wait(t, "ordinary seal", func() bool {
					repo, err := f.S.Repository(target)
					fixturekit.Must(t, err)
					_, _, found, err := seal.Latest(repo, f.Stream1)
					fixturekit.Must(t, err)
					return found
				})
				f.Stop(t)
				f.Start(t)
				checkHandInRetry(t, f, req)
				repo, err := f.S.Repository(target)
				fixturekit.Must(t, err)
				_, _, found, err := seal.Latest(repo, f.Stream1)
				fixturekit.Must(t, err)
				if !found {
					t.Fatal("restart lost ordinary seal")
				}
				if _, found, err := intake.DesignSelectionOf(repo, f.Origin); err != nil || found {
					t.Fatalf("single-project hand-in recorded a selection: found=%v error=%v", found, err)
				}
				chief, host, _, closeChief, err := f.S.ChiefRepository(context.Background(), repo, f.Stream1)
				fixturekit.Must(t, err)
				thread, err := chief.Thread(host, trace.Manager)
				closeChief()
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(thread.Identity, f.Before.Identity) {
					t.Fatal("single selection replaced the hand-in chief")
				}
				status, err := f.C.Status(context.Background(), f.Stream1)
				fixturekit.Must(t, err)
				if status.Project != target || status.Workgroup == nil || !slices.Equal(status.Workgroup.Members, []config.WorkstreamID{f.Stream1}) || len(status.Workgroup.Edges) != 0 {
					t.Fatalf("single selection does not present one workstream in the selected repo: %+v", status)
				}
				source, err := f.C.Conversation(context.Background(), f.Origin)
				fixturekit.Must(t, err)
				member, err := f.C.Conversation(context.Background(), f.Stream1)
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(source.Entries, member.Entries) {
					t.Fatal("single selection split the conversation")
				}

			})
		}
	}
}

func TestHandInProductionMultipleReposPreservesSkippedDebate(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	req := handInProduction(t, f, api.HandInRequest{SkipDebate: true})
	var packet api.PacketResponse
	f.Wait(t, "skipped multi-repo packet", func() bool {
		got, err := f.C.Packet(context.Background(), f.Origin)
		if err != nil || got.ProjectSpec == nil {
			return false
		}
		packet = got
		return true
	})
	if !packet.Packet.Skipped {
		t.Fatal("allocation lost the owner's skip")
	}
	for i, m := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		project := f.Project
		if i == 1 {
			project = f.Project2
		}
		repo, err := f.S.Repository(project)
		fixturekit.Must(t, err)
		threads, err := repo.Threads(m)
		fixturekit.Must(t, err)
		if slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.Role == intake.CommitteeRole }) {
			t.Fatal("committee ran despite hand-in skip")
		}
	}
	_, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	checkHandInRetry(t, f, req)
	f.CheckContinuation(t)
}

func TestHandInProductionSharedChiefResumesBothQuestionsAfterRestart(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	handInProduction(t, f, api.HandInRequest{})
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	f.Engine.Mu.Lock()
	f.Engine.Turns["handin-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "Should acknowledgments retain their offsets?"})
		return intakefixture.QuestionResult(req, "session-ask", "Asked the shared chief."), err
	}
	f.Engine.Turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, "Keep the acknowledged offset.") {
			return nil, errors.New("answer went to the wrong asking turn")
		}
		return intakefixture.QuestionResult(req, "session-resumed", "Resumed within the contract."), nil
	}
	f.Engine.Mu.Unlock()
	f.SharedDesign().LifecycleWorker(t, f.Project, f.Stream1, "worker_first", "handin-ask", "Follow the server slice.")
	f.SharedDesign().LifecycleWorker(t, f.Project2, f.Stream2, "worker_second", "handin-ask", "Follow the client slice.")
	originals := make([]trace.Question, 2)
	f.Wait(t, "both open member questions", func() bool {
		ready := true
		for i, m := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			project := f.Project
			if i == 1 {
				project = f.Project2
			}
			repo, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			q, err := trace.Read[trace.Question](repo, m)
			fixturekit.Must(t, err)
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
	f.Stop(t)
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, questions.AnswerTool) {
			for _, m := range []config.WorkstreamID{f.Stream1, f.Stream2} {
				if _, err := fixturekit.CallTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(m) + ":1", "text": "Keep the acknowledged offset.", "citations": []string{"spec#1"}}); err != nil {
					return nil, err
				}
			}
		}
		return intakefixture.QuestionResult(req, "session-chief", "Both member questions were answered."), nil
	}
	f.Engine.Mu.Unlock()
	f.Start(t)
	f.Wait(t, "both original askers resumed", func() bool {
		for i, m := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			project := f.Project
			worker := "worker_first"
			if i == 1 {
				project = f.Project2
				worker = "worker_second"
			}
			repo, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			th, err := repo.Thread(m, worker)
			fixturekit.Must(t, err)
			if len(th.Turns) < 2 || th.Turns[1].CompletedAt.IsZero() {
				return false
			}
			if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != "answer_1" || th.Turns[1].Status() != "idle" {
				t.Fatalf("duplicated or failed continuation: %+v", th)
			}
			q, err := trace.Get[trace.Question](repo, m, "1", 1)
			fixturekit.Must(t, err)
			if !intakefixture.EqualJSONTest(q, originals[i]) {
				t.Fatal("restart changed original asking links")
			}
		}
		return true
	})
	f.CheckContinuation(t)
}

func TestHandInProductionPreservesSameRepoBaseOnSelectedMember(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	parent := intake.HandInWorkstream(f.Project, "parent")
	f.Engine.Mu.Lock()
	draft := f.Engine.Turns["draft-1-1"]
	f.Engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Workspace.Directory(), string(parent)) {
			return draft(ctx, req, verified, tools)
		}
		for path, content := range map[string]string{"spec.md": fixturekit.ValidSpec, "plan.json": fixturekit.ValidPlan} {
			if _, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": path, "content": content}); err != nil {
				return nil, err
			}
		}
		return intakefixture.QuestionResult(req, "session-parent-draft", "Parent drafted."), nil
	}
	f.Engine.Mu.Unlock()
	text := "Implement the same-repo prerequisite."
	_, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: "parent", Stdin: &text, SkipDebate: true})
	fixturekit.Must(t, err)
	var parentPacket api.PacketResponse
	f.Wait(t, "parent packet", func() bool {
		got, err := f.C.Packet(context.Background(), parent)
		if err != nil {
			return false
		}
		parentPacket = got
		return true
	})
	_, err = f.C.Ratify(context.Background(), parent, parentPacket.Packet.Revision.Spec, parentPacket.Packet.Revision.Plan)
	fixturekit.Must(t, err)
	f.Wait(t, "parent seal", func() bool {
		repo, err := f.S.Repository(f.Project)
		fixturekit.Must(t, err)
		_, _, ok, err := seal.Latest(repo, parent)
		fixturekit.Must(t, err)
		return ok
	})
	req := handInProduction(t, f, api.HandInRequest{Base: parent})
	packet := f.AwaitPacket(t)
	_, err = f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.Wait(t, "dependent group seals", func() bool {
		for i, m := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			project := f.Project
			if i == 1 {
				project = f.Project2
			}
			repo, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			record, _, found, err := seal.Latest(repo, m)
			fixturekit.Must(t, err)
			if !found {
				return false
			}
			base, err := repo.WorkstreamBase(m)
			fixturekit.Must(t, err)
			if i == 0 {
				if base.Base != parent || base.Revision != 1 || record.Base.Workstream != parent || record.Base.Branch != intake.FeatureBranch(parent) {
					t.Fatalf("same-repo base changed during allocation: %+v %+v", base, record.Base)
				}
			} else if base.Base != "" || record.Base.Workstream != "" {
				t.Fatalf("base leaked into another repo: %+v %+v", base, record.Base)
			}
		}
		return true
	})
	f.Stop(t)
	f.Start(t)
	checkHandInRetry(t, f, req)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	base, err := repo.WorkstreamBase(f.Stream1)
	fixturekit.Must(t, err)
	if base.Base != parent || base.Revision != 1 {
		t.Fatalf("restart changed the original base pin: %+v", base)
	}
}

func TestHandInOwnerProjectsValidationRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	before := map[config.ProjectID][]config.WorkstreamID{}
	for _, p := range []config.ProjectID{f.Project, f.Project2} {
		r, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		before[p], err = r.Workstreams()
		fixturekit.Must(t, err)
	}
	for _, projects := range [][]config.ProjectID{{}, {f.Project, f.Project}, {f.Project, "p_0123456789abcdef0123456789abcdef"}} {
		text := intakefixture.ProductionBrief
		if apiErr := handInError(t, f.C, api.HandInRequest{Projects: projects, Key: "invalid", Stdin: &text}); apiErr.Code != api.Validation {
			t.Fatalf("projects %v: %+v", projects, apiErr)
		}
	}
	for p, want := range before {
		r, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		got, err := r.Workstreams()
		fixturekit.Must(t, err)
		if !slices.Equal(got, want) {
			t.Fatalf("refused hand-ins allocated work: %v -> %v", want, got)
		}
	}
}

func TestHandInOwnerProjectsKeepPausedMemberWaiting(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "project", Project: f.Project2}, Mode: "soft", Source: "owner"})
	handInProduction(t, f, api.HandInRequest{SkipDebate: true})
	f.Wait(t, "active member drafts while paused member waits", func() bool {
		a, err := f.S.Repository(f.Project)
		fixturekit.Must(t, err)
		state, err := a.Workflow(f.Stream1, trace.FeatureSubject)
		if err != nil {
			return false
		}
		if state.Value != intake.SketchedState && state.Value != intake.InShedState {
			return false
		}
		b, err := f.S.Repository(f.Project2)
		fixturekit.Must(t, err)
		state, err = b.Workflow(f.Stream2, trace.FeatureSubject)
		fixturekit.Must(t, err)
		if state.Value != intake.HandedState {
			t.Fatalf("paused member advanced: %+v", state)
		}
		th, err := b.Threads(f.Stream2)
		fixturekit.Must(t, err)
		if slices.ContainsFunc(th, func(th trace.Thread) bool { return th.Identity.Role == "architect" && len(th.Turns) > 0 }) {
			t.Fatal("paused member ran an architect")
		}
		return true
	})
}

func TestHandInOwnerBaseAppliesOnlyToItsSelectedProject(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	b, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	base := intake.HandInWorkstream(f.Project2, "base")
	fixturekit.Must(t, b.CreateWorkstreamOn(context.Background(), base, config.WorkspacesGit, f.Clock.Now(), intake.OwnerActor))
	text := intakefixture.ProductionBrief
	if apiErr := handInError(t, f.C, api.HandInRequest{Projects: []config.ProjectID{f.Project}, Key: "base-refused", Stdin: &text, Base: base}); apiErr.Code != api.Validation {
		t.Fatalf("unselected base: %+v", apiErr)
	}
	handInProduction(t, f, api.HandInRequest{Base: base, SkipDebate: true})
	f.Wait(t, "base pinned to its member", func() bool {
		pin, err := b.WorkstreamBase(f.Stream2)
		if err != nil || pin.Base == "" {
			return false
		}
		if pin.Base != base || pin.Revision != 1 {
			t.Fatalf("other-project base: %+v", pin)
		}
		a, err := f.S.Repository(f.Project)
		fixturekit.Must(t, err)
		pin, err = a.WorkstreamBase(f.Stream1)
		fixturekit.Must(t, err)
		if pin.Base != "" {
			t.Fatalf("base leaked into home member: %+v", pin)
		}
		return true
	})
}

func TestHandInOwnerProjectListCannotBeChanged(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	req := handInProduction(t, f, api.HandInRequest{})
	f.AwaitPacket(t)
	for _, projects := range [][]config.ProjectID{{f.Project}, {f.Project, f.Project2, f.Project}} {
		req.Projects = projects
		apiErr := handInError(t, f.C, req)
		if apiErr.Code != api.Conflict && apiErr.Code != api.Validation {
			t.Fatalf("changed list: %+v", apiErr)
		}
	}
	for _, kind := range []string{"amend", "cancel"} {
		_, apiErr := service.InspectForTest(f.S.Core()).LifecycleController().RequestChange(context.Background(), string(f.Origin), lifecycle.ChangeRequest{Key: "membership-" + kind, Kind: kind, Scope: "workstream", Workstream: f.Stream2, Change: "Remove the second member"})
		if apiErr == nil || apiErr.Code != api.Conflict {
			t.Fatalf("%s before ratification: %v", kind, apiErr)
		}
		if kind == "cancel" && !strings.Contains(apiErr.Message, "cancel the whole workgroup") {
			t.Fatalf("cancellation refusal lacks the abandonment path: %v", apiErr)
		}
	}
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	_, g, _, err := r.CrossRepoProjectSpec(config.WorkgroupID("g_" + strings.TrimPrefix(string(f.Origin), "w_")))
	fixturekit.Must(t, err)
	if !slices.Equal(g.Members, []config.WorkstreamID{f.Origin, f.Stream1, f.Stream2}) {
		t.Fatalf("members changed: %+v", g)
	}
	_, err = f.C.Abandon(context.Background(), f.Stream2, "Drop just this member")
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("abandon before ratification: %v", err)
	}
	_, err = f.C.MoveWorkstream(context.Background(), f.Stream2, intake.AbandonedState, "Drop this member through a move")
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("abandonment move before ratification: %v", err)
	}
	applyHandInCancellation(t, f, lifecycle.ChangeRequest{Key: "abandon-whole", Kind: "cancel", Scope: "workgroup", Change: "Abandon this selection and hand in again"})
	for _, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		_, _, repo, apiErr := f.S.Intake().ConversationTrace(string(member))
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		state, err := repo.Workflow(member, trace.FeatureSubject)
		fixturekit.Must(t, err)
		if state.Value != intake.AbandonedState {
			t.Fatalf("whole-workgroup cancellation left member live: %s %+v", member, state)
		}
	}
}

func applyHandInCancellation(t *testing.T, f *journey.ProductionDesignFixture, req lifecycle.ChangeRequest) {
	t.Helper()
	controller := service.InspectForTest(f.S.Core()).LifecycleController()
	ctx := context.Background()
	change, apiErr := controller.RequestChange(ctx, string(f.Origin), req)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	wt, group, _, closeGroup, err := controller.ChangeContext(ctx, string(f.Origin))
	fixturekit.Must(t, err)
	defer closeGroup()
	f.Wait(t, "cancellation impact presented", func() bool {
		fixturekit.Must(t, controller.AdvanceChanges(ctx, wt, group))
		changes, err := lifecycle.ReadChanges(wt, group.Members[0])
		fixturekit.Must(t, err)
		for _, recorded := range changes {
			if recorded.ID == change.ID {
				change = recorded
			}
		}
		return change.State == "presented"
	})
	change, apiErr = controller.DecideChange(ctx, string(f.Origin), change.ID, lifecycle.ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "close"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	f.Wait(t, "approved cancellation applied", func() bool {
		fixturekit.Must(t, controller.AdvanceChanges(ctx, wt, group))
		changes, err := lifecycle.ReadChanges(wt, group.Members[0])
		fixturekit.Must(t, err)
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
	handInProduction(t, f, api.HandInRequest{})
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	applyHandInCancellation(t, f, lifecycle.ChangeRequest{Key: "cancel-ratified-member", Kind: "cancel", Scope: "workstream", Workstream: f.Stream2, Change: "Cancel just the client after ratification"})
	for _, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		_, _, repo, apiErr := f.S.Intake().ConversationTrace(string(member))
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		state, err := repo.Workflow(member, trace.FeatureSubject)
		fixturekit.Must(t, err)
		if (state.Value == intake.AbandonedState) != (member == f.Stream2) {
			t.Fatalf("post-ratification member cancellation: %s %+v", member, state)
		}
	}
	repo, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- repo.WithMemberQuestionSnapshot(f.Stream2, func(trace.MemberQuestionSnapshot) error {
			close(held)
			<-release
			return nil
		})
	}()
	defer func() {
		close(release)
		fixturekit.Must(t, <-finished)
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("member retirement snapshot did not reach its boundary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := f.C.Abandon(ctx, f.Stream1, "Cancel the ratified survivor while the other member retires")
	fixturekit.Must(t, err)
	if out.State != intake.AbandonedState {
		t.Fatalf("ratified survivor did not abandon: %+v", out)
	}
}

func TestArchitectNoChangeEscalatesWithSelectedMemberPresent(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{HandIn: true})
	const question = "The selected client project needs no change. Ask the owner whether to abandon and hand in again."
	f.Engine.Mu.Lock()
	draft := f.Engine.Turns["draft-1-1"]
	f.Engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.SystemPrompt, "Never add or drop a member") || !strings.Contains(req.SystemPrompt, "escalate that judgment to the owner") {
			return nil, errors.New("architect lacks fixed-member instructions")
		}
		if !strings.Contains(req.Workspace.Directory(), string(f.Stream2)) {
			return draft(ctx, req, verified, tools)
		}
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
		return intakefixture.QuestionResult(req, "session-no-change", "Asked the chief to escalate."), err
	}
	original := f.Engine.Turns["*"]
	var escalated atomic.Bool
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, questions.EscalateTool) && !escalated.Load() {
			r, err := f.S.Repository(f.Project2)
			if err != nil {
				return nil, err
			}
			qs, err := trace.Read[trace.Question](r, f.Stream2)
			if err == nil && len(qs) > 0 {
				_, err = fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.Stream2) + ":1"}, "rephrasing": question, "blocked": "Drafting the selected client member.", "options": []string{"Keep this selection", "Abandon and hand in again"}, "recommendation": "Keep the member; ask the owner to abandon and hand in again if the selection should change."})
				if err == nil {
					escalated.Store(true)
				}
				return intakefixture.QuestionResult(req, "session-chief-no-change", "Escalated the no-change judgment."), err
			}
		}
		return original(ctx, req, verified, tools)
	}
	f.Engine.Mu.Unlock()
	handInProduction(t, f, api.HandInRequest{})
	f.Wait(t, "no-change judgment in the owner's inbox", func() bool {
		inbox, err := f.C.Inbox(context.Background())
		fixturekit.Must(t, err)
		for _, e := range inbox.Entries {
			if e.Kind == "escalation" && strings.Contains(e.Question, "needs no change") {
				r, err := f.S.Repository(f.Project)
				fixturekit.Must(t, err)
				id, err := r.CrossRepoWorkgroupOf(f.Origin)
				fixturekit.Must(t, err)
				_, g, _, err := r.CrossRepoProjectSpec(id)
				fixturekit.Must(t, err)
				if !slices.Contains(g.Members, f.Stream2) || len(g.Members) != 3 {
					t.Fatalf("no-change member was dropped: %+v", g)
				}
				return true
			}
		}
		return false
	})
}
