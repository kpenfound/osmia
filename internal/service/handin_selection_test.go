package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newHandInProductionFixture(t *testing.T) *productionDesignFixture {
	t.Helper()
	return newConfiguredProductionDesignFixture(t, productionDesignConfiguration{handIn: true, wait: lifecycleAwait})
}

// handInProduction uses the public API. Only the injected selection engine
// waits while the test captures the existing conversation; Go owns the handoff.
func handInProduction(t *testing.T, f *productionDesignFixture, req HandInRequest) HandInRequest {
	t.Helper()
	ready := make(chan struct{})
	defer close(ready)
	f.engine.mu.Lock()
	selection := f.engine.turns["select-repos-1"]
	f.engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !strings.HasSuffix(req.Prompt, productionBrief) || !strings.Contains(req.SystemPrompt, string(f.project)) || !strings.Contains(req.SystemPrompt, string(f.project2)) {
			return nil, errors.New("selection lacks the immutable input or project repo context")
		}
		return selection(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()
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

func TestHandInProductionSelectsDraftsRatifiesAndRestarts(t *testing.T) {
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
				fake.mu.Lock()
				fake.text = map[issues.Ref]string{{Owner: "owner", Repo: "repo", Number: 12}: productionBrief}
				fake.mu.Unlock()
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
			// Retry does not reread the file or refetch the issue.
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
				fake.mu.Lock()
				count := fake.fetches
				fake.mu.Unlock()
				if count != 1 {
					t.Fatalf("issue fetched %d times", count)
				}
			}
		})
	}
}

func TestHandInProductionRecoversSelectionAllocationJoinAndSeal(t *testing.T) {
	for _, boundary := range []struct {
		step string
		nth  int32
	}{
		{"design-selection-recorded", 1}, {"design-member-allocated", 1}, {"design-member-allocated", 2},
		{"design-workgroup-prepared", 1}, {"design-member-joined", 1}, {"design-member-joined", 2},
		{"design-conversation-imported", 1}, {"design-conversation-adopted", 1}, {"group-member-seal-prepared", 1},
	} {
		t.Run(fmt.Sprintf("%s-%d", boundary.step, boundary.nth), func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			hook := &designInterruption{step: boundary.step, nth: boundary.nth, reached: make(chan struct{})}
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
			case <-hook.reached:
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
			f.stop(t)
			if ratified != nil {
				<-ratified
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

func TestHandInProductionRefusesInvalidSelectionBeforeAllocation(t *testing.T) {
	for _, kind := range []string{"empty", "duplicate", "outside", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			selected := []string{}
			switch kind {
			case "duplicate":
				selected = []string{string(f.project), string(f.project)}
			case "outside":
				selected = []string{"p_000000000000000000000000000000ff"}
			case "invalid":
				selected = []string{"invalid"}
			}
			f.engine.mu.Lock()
			f.engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if _, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": selected}); err == nil {
					return nil, errors.New("invalid selection accepted")
				}
				return questionResult(req, "session-refused", "Selection refused."), nil
			}
			f.engine.mu.Unlock()
			req := handInProduction(t, f, HandInRequest{})
			f.wait(t, "refused selection", func() bool {
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
						t.Fatalf("invalid selection allocated %s", m)
					}
				}
				_, found, err := designSelectionOf(repo, f.origin)
				if p == f.project {
					must(t, err)
					if found {
						t.Fatal("invalid selection pinned")
					}
				}
			}
			checkHandInRetry(t, f, req)
			f.stop(t)
			f.start(t)
			checkHandInRetry(t, f, req)
		})
	}
}

func TestHandInProductionSingleSelectedRepoKeepsOrdinaryPacket(t *testing.T) {
	for _, other := range []bool{false, true} {
		for _, skip := range []bool{false, true} {
			name := "original"
			if other {
				name = "other"
			}
			if skip {
				name += "-skip"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newHandInProductionFixture(t)
				target := f.project
				f.stream1 = f.origin
				if other {
					target = f.project2
					f.stream1 = f.stream2
				}
				f.engine.mu.Lock()
				f.engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
					_, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": []string{string(target)}})
					return questionResult(req, "session-single", "One repo is affected."), err
				}
				for i := 1; i <= 2; i++ {
					f.engine.turns[roundTurnID(1, committeeAgent(i), 1)] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
						return questionResult(req, "session-committee-"+req.Name, "The single-repo draft fits the charter."), nil
					}
				}
				f.engine.mu.Unlock()
				req := handInProduction(t, f, HandInRequest{SkipDebate: skip})
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
	f.engine.mu.Lock()
	f.engine.turns["handin-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "Should acknowledgments retain their offsets?"})
		return questionResult(req, "session-ask", "Asked the shared chief."), err
	}
	f.engine.turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, "Keep the acknowledged offset.") {
			return nil, errors.New("answer went to the wrong asking turn")
		}
		return questionResult(req, "session-resumed", "Resumed within the contract."), nil
	}
	f.engine.mu.Unlock()
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
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
	f.engine.mu.Unlock()
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
	f.engine.mu.Lock()
	selectBoth := f.engine.turns["select-repos-1"]
	f.engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Workspace.Directory(), string(parent)) {
			_, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": []string{string(f.project)}})
			return questionResult(req, "session-parent", "The parent affects only its repo."), err
		}
		return selectBoth(ctx, req, verified, tools)
	}
	draft := f.engine.turns["draft-1-1"]
	f.engine.turns["draft-1-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
	f.engine.mu.Unlock()
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
