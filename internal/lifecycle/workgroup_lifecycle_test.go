package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newLifecycleDesignFixture(t *testing.T) (*journey.ProductionDesignFixture, *journey.LifecycleCleanupGate) {
	t.Helper()
	gate := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(gate.Release)
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{
		Wait: journey.LifecycleAwait, Checkpoint: gate.Checkpoint,
	})
	return f, gate
}

func lifecycleWorker(t *testing.T, f *journey.ProductionDesignFixture, project config.ProjectID, stream config.WorkstreamID, worker, turn, prompt string) {
	t.Helper()
	f.SharedDesign().LifecycleWorker(t, project, stream, worker, turn, prompt)
}
func TestDesignProductionSharedLifecycleAfterArchiveCleanupAndRestart(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"source-repo", "least-member"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, cleanupGate := newLifecycleDesignFixture(t)
			defer func() { f.Stop(t) }()
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			f.AwaitApproved(t)
			retired, survivor := f.Stream1, f.Stream2
			retiredProject, survivorProject := f.Project, f.Project2
			retiredWorker, survivorWorker := "worker_first", "worker_second"
			if choice == "least-member" && lifecycle.LeastWorkstream([]config.WorkstreamID{f.Stream1, f.Stream2}) == f.Stream2 {
				retired, survivor = f.Stream2, f.Stream1
				retiredProject, survivorProject = f.Project2, f.Project
				retiredWorker, survivorWorker = "worker_second", "worker_first"
			}
			memberRepo, err := f.S.Repository(survivorProject)
			fixturekit.Must(t, err)
			first, host, _, closeFirst, err := f.S.Lifecycle().ChiefRepository(ctx, memberRepo, survivor)
			fixturekit.Must(t, err)
			defer func() { closeFirst() }()
			second, _, _, closeSecond, err := f.S.Lifecycle().ChiefRepository(ctx, memberRepo, survivor)
			fixturekit.Must(t, err)
			defer func() { closeSecond() }()
			if first != second {
				t.Fatal("overlapping handles created different authority")
			}
			groups, _, err := trace.Workgroups(first)
			fixturekit.Must(t, err)
			group := groups[0]
			approval, approvalDoc, found, err := trace.ReadGroupApproval(first, host)
			fixturekit.Must(t, err)
			if !found || !approval.Complete {
				t.Fatal("production effective seals did not complete")
			}
			unrelated := intake.HandInWorkstream(f.Project, "lifecycle-unrelated")
			var engineMu sync.Mutex
			var batchEscalated bool
			var held chan struct{}
			var entered chan struct{}
			f.Engine.Mu.Lock()
			f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				names, err := intakefixture.ToolNames(ctx, tools)
				if err != nil {
					return nil, err
				}
				if strings.Contains(req.SessionDir, string(unrelated)) && slices.Contains(names, status.ToolName) {
					_, err := fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "An independent request.", "note": "Waiting on its own ratification.", "agents": []string{}})
					return intakefixture.QuestionResult(req, "session-unrelated-chief", "Independent work remains available."), err
				}
				if !slices.Contains(names, status.ToolName) {
					return intakefixture.QuestionResult(req, "session-idle", "Working within the slice."), nil
				}
				if strings.Contains(req.Prompt, "lifecycle-owner-") {
					engineMu.Lock()
					gate, reached := held, entered
					entered = nil
					engineMu.Unlock()
					if reached != nil {
						close(reached)
						select {
						case <-gate:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
				}
				engineMu.Lock()
				needBatch := !batchEscalated
				engineMu.Unlock()
				if needBatch {
					retiredRepo, err := f.S.Repository(retiredProject)
					if err != nil {
						return nil, err
					}
					survivorRepo, err := f.S.Repository(survivorProject)
					if err != nil {
						return nil, err
					}
					left, err := retiredRepo.Questions(retired)
					if err != nil {
						return nil, err
					}
					right, err := survivorRepo.Questions(survivor)
					if err != nil {
						return nil, err
					}
					if len(left) > 0 && len(right) > 0 && left[0].State == trace.QuestionOpen && right[0].State == trace.QuestionOpen {
						_, err = fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(retired) + ":" + left[0].Asked.ID, string(survivor) + ":" + right[0].Asked.ID}, "rephrasing": "A shared contract decision before retirement.", "blocked": "Both original asking turns.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
						if err != nil {
							return nil, err
						}
						engineMu.Lock()
						batchEscalated = true
						engineMu.Unlock()
					}
				}
				repo, err := f.S.Repository(survivorProject)
				if err != nil {
					return nil, err
				}
				qs, err := repo.Questions(survivor)
				if err != nil {
					return nil, err
				}
				for _, q := range qs {
					id := string(survivor) + ":" + q.Asked.ID
					if q.State == trace.QuestionOpen && strings.HasPrefix(q.Asked.Question, "direct-") {
						_, err = fixturekit.CallTool(ctx, tools, questions.AnswerTool, map[string]any{"question": id, "text": "Keep the agreed contract.", "citations": []string{"spec#1"}})
					}
					if q.State == trace.QuestionOpen && strings.HasPrefix(q.Asked.Question, "escalate-") {
						_, err = fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{id}, "rephrasing": q.Asked.Question, "blocked": "The asking origin.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
					}
					if q.State == trace.QuestionRuled {
						_, err = fixturekit.CallTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": id, "text": q.Ruling.OwnerResponse, "scope": "local"})
					}
					if err != nil {
						return nil, err
					}
				}
				_, err = fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Continue the surviving slice.", "note": "The project contract is active.", "agents": []string{}})
				return intakefixture.QuestionResult(req, "session-chief", "The survivor keeps its original conversation."), err
			}
			f.Engine.Turns["lifecycle-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				question := "consolidated-survivor"
				if strings.Contains(req.SessionDir, retiredWorker) {
					question = "consolidated-origin"
				}
				_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
				return intakefixture.QuestionResult(req, "session-worker", "Asked the shared chief."), err
			}
			for _, boundary := range []string{"retained", "deleted", "restarted"} {
				for _, kind := range []string{"ask", "escalate"} {
					question := "direct-" + boundary
					if kind == "escalate" {
						question = "escalate-" + boundary
					}
					f.Engine.Turns[kind+"-"+boundary] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
						_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
						return intakefixture.QuestionResult(req, "session-worker", "Asked the shared chief."), err
					}
				}
			}
			primaryDraft := f.Engine.Turns["draft-1-1"]
			f.Engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if !strings.Contains(req.Workspace.Directory(), string(unrelated)) {
					return primaryDraft(ctx, req, turn, tools)
				}
				for _, doc := range []struct{ path, text string }{{"spec.md", intakefixture.CrossSpec1}, {"plan.json", intakefixture.CrossPlan1}} {
					if _, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": doc.path, "content": doc.text}); err != nil {
						return nil, err
					}
				}
				return intakefixture.QuestionResult(req, "session-unrelated-architect", "Independent draft delivered."), nil
			}
			f.Engine.Mu.Unlock()
			brief := "Build independent chunk storage."
			other, err := f.C.HandIn(ctx, api.HandInRequest{Project: f.Project, Key: "lifecycle-unrelated", Stdin: &brief, SkipDebate: true})
			fixturekit.Must(t, err)
			if other.Workstream != unrelated {
				t.Fatal("independent hand-in identity changed")
			}
			journey.LifecycleAwait(t, f, "independent owner packet", func() bool {
				packet, err := f.C.Packet(ctx, unrelated)
				return err == nil && packet.ProjectSpec == nil
			})
			otherRepo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			otherChief, err := otherRepo.ChiefOfStaffThread(unrelated)
			fixturekit.Must(t, err)
			lifecycleWorker(t, f, retiredProject, retired, retiredWorker, "lifecycle-ask", "consolidated-origin")
			lifecycleWorker(t, f, survivorProject, survivor, "worker_unaffected", "lifecycle-ask", "consolidated-survivor")
			var originalEntry api.InboxEntry
			journey.LifecycleAwait(t, f, "consolidated production escalation", func() bool {
				inbox, err := f.C.Inbox(ctx)
				fixturekit.Must(t, err)
				for _, e := range inbox.Entries {
					if e.Question == "A shared contract decision before retirement." {
						if len(e.Asked) != 2 || e.Workstream != retired || slices.ContainsFunc(e.Asked, func(q api.InboxQuestion) bool { return q.ID == "" }) {
							return false
						}
						originalEntry = e
						return true
					}
				}
				return false
			})
			originalQuestions, err := first.Questions(survivor)
			fixturekit.Must(t, err)

			_, err = f.C.Abandon(ctx, retired, "The owner cancelled this member.")
			fixturekit.Must(t, err)
			_, err = f.C.Archive(ctx, retired)
			fixturekit.Must(t, err)
			cleanupGate.Await(t, f)
			resumed := func(repo *trace.Repository, q trace.QuestionState) bool {
				worker, err := repo.Thread(survivor, q.Asked.AskedBy.ID)
				fixturekit.Must(t, err)
				count := 0
				done := false
				for _, turn := range worker.Turns {
					if turn.Request.TurnID == questions.TurnID(q.Asked.ID) {
						count++
						done = turn.Response != nil && !turn.CompletedAt.IsZero() && turn.Status() == "idle" && strings.Contains(turn.Request.Prompt, q.Ruling.ReturnedAnswer)
					}
				}
				if count > 1 {
					t.Fatal("duplicate asking-turn continuation")
				}
				return done
			}
			check := func(boundary string) {
				t.Helper()
				repo, err := f.S.Repository(survivorProject)
				fixturekit.Must(t, err)
				target, currentHost, _, release, err := f.S.Lifecycle().ChiefRepository(ctx, repo, survivor)
				fixturekit.Must(t, err)
				defer release()
				inbox, err := f.C.Inbox(ctx)
				fixturekit.Must(t, err)
				matches := 0
				for _, e := range inbox.Entries {
					if e.Number != originalEntry.Number || e.Project != originalEntry.Project {
						continue
					}
					matches++
					if e.Workstream != originalEntry.Workstream || e.Batch != originalEntry.Batch || !intakefixture.EqualJSONTest(e.Asked, originalEntry.Asked) || !strings.Contains(e.Blocked, "operation remains held") {
						t.Fatalf("retired first origin lost original authority: got %+v; original %+v", e, originalEntry)
					}
				}
				if matches != 1 {
					t.Fatal("consolidated entry disappeared or duplicated")
				}
				_, err = f.C.Answer(ctx, originalEntry.Number, "Keep the agreed contract.", originalEntry.Project)
				if err == nil || !strings.Contains(err.Error(), "operation remains held") {
					t.Fatalf("unavailable effect did not remain explicitly held: %v", err)
				}
				currentQuestions, err := repo.Questions(survivor)
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(originalQuestions[0], currentQuestions[0]) {
					t.Fatal("held ruling changed pending survivor origin")
				}
				if currentHost != host {
					t.Fatal("conversation host changed")
				}
				got, doc, found, err := trace.ReadGroupApproval(target, host)
				fixturekit.Must(t, err)
				if !found || !intakefixture.EqualJSONTest(got, approval) || !intakefixture.EqualJSONTest(doc, approvalDoc) {
					t.Fatal("approval pins changed across retirement")
				}
				active, err := trace.ProjectSpecActive(target, group.ID)
				fixturekit.Must(t, err)
				if !active {
					t.Fatal("one archived member resolved the group")
				}
				b, err := f.S.Context().Assemble(ctx, survivorProject, bundle.Scope{Role: trace.ChiefOfStaff, Workstream: survivor})
				fixturekit.Must(t, err)
				if !strings.Contains(b.Brief(), "acknowledgment") {
					t.Fatal("survivor lost active contracts")
				}
				th, err := target.Thread(host, trace.ChiefOfStaff)
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(f.Before.Identity, th.Identity) || !intakefixture.EqualJSONTest(f.Before.Turns, th.Turns[:len(f.Before.Turns)]) {
					t.Fatal("adopted conversation changed")
				}
				if _, err := target.Workflow(retired, trace.FeatureSubject); !errors.Is(err, trace.ErrMemberRetired) {
					t.Fatalf("retired member must hold: %v", err)
				}
				engineMu.Lock()
				held, entered = make(chan struct{}), make(chan struct{})
				gate, reached := held, entered
				engineMu.Unlock()
				sent, err := f.C.Send(ctx, survivor, "lifecycle-owner-"+boundary)
				fixturekit.Must(t, err)
				select {
				case <-reached:
				case <-time.After(fixturekit.DemoTimeout):
					close(gate)
					t.Fatal("live shared dispatch did not reach owner turn")
				}
				reads := make(chan error, 4)
				readCtx, cancel := context.WithTimeout(ctx, fixturekit.DemoTimeout)
				defer cancel()
				go func() { _, err := f.C.Conversation(readCtx, survivor); reads <- err }()
				go func() {
					var feed api.FeedResponse
					reads <- f.C.Do(readCtx, "GET", api.Prefix+"/feed/"+string(survivor), nil, &feed)
				}()
				go func() { _, _, _, err := repo.CrossRepoProjectSpec(group.ID); reads <- err }()
				go func() { _, err := f.C.Send(readCtx, survivor, "Queued while the chief is working."); reads <- err }()
				for range 4 {
					select {
					case err := <-reads:
						if err != nil {
							close(gate)
							t.Fatal(err)
						}
					case <-readCtx.Done():
						close(gate)
						t.Fatal("owner access blocked behind chief turn")
					}
				}
				close(gate)
				journey.LifecycleAwait(t, f, "owner turn "+boundary, func() bool {
					conversation, err := f.C.Conversation(ctx, survivor)
					fixturekit.Must(t, err)
					return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
						return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone
					})
				})
				lifecycleWorker(t, f, survivorProject, survivor, survivorWorker, "ask-"+boundary, "direct-"+boundary)
				journey.LifecycleAwait(t, f, "direct member answer "+boundary, func() bool {
					qs, err := repo.Questions(survivor)
					fixturekit.Must(t, err)
					return slices.ContainsFunc(qs, func(q trace.QuestionState) bool {
						return q.Asked.Question == "direct-"+boundary && q.State == trace.QuestionAnswered && q.Ruling != nil && resumed(repo, q)
					})
				})

				journey.LifecycleAwait(t, f, "survivor event acknowledgment "+boundary, func() bool {
					outbox, err := repo.Outbox(survivor)
					fixturekit.Must(t, err)
					chief, err := target.Thread(host, trace.ChiefOfStaff)
					fixturekit.Must(t, err)
					for _, e := range outbox {
						if e.Event.Operation != nil || !strings.Contains(e.Event.Body, "direct-"+boundary) {
							continue
						}
						if !e.Acknowledged {
							return false
						}
						acknowledgments := 0
						claims := map[string]bool{}
						for _, action := range e.History {
							if action.Kind == "acknowledge" {
								acknowledgments++
							}
							if action.Kind == "claim" {
								claims[events.TurnID(action.Token)] = true
							}
						}
						delivered := 0
						for _, turn := range chief.Turns {
							if claims[turn.Request.TurnID] && !turn.CompletedAt.IsZero() && turn.Status() == "idle" {
								delivered++
							}
						}
						if acknowledgments != 1 || delivered != 1 {
							t.Fatalf("event delivered or acknowledged more than once: acks=%d deliveries=%d", acknowledgments, delivered)
						}
						return true
					}
					return false
				})
				lifecycleWorker(t, f, survivorProject, survivor, survivorWorker, "escalate-"+boundary, "escalate-"+boundary)
				var entry api.InboxEntry
				journey.LifecycleAwait(t, f, "survivor escalation "+boundary, func() bool {
					inbox, err := f.C.Inbox(ctx)
					fixturekit.Must(t, err)
					matches := 0
					for _, e := range inbox.Entries {
						if e.Question == "escalate-"+boundary {
							entry = e
							matches++
						}
					}
					if matches > 1 {
						t.Fatal("duplicate escalation")
					}
					return matches == 1
				})
				_, err = f.C.Answer(ctx, entry.Number, "Keep the agreed contract.", entry.Project)
				fixturekit.Must(t, err)
				journey.LifecycleAwait(t, f, "ruling continuation "+boundary, func() bool {
					qs, err := repo.Questions(survivor)
					fixturekit.Must(t, err)
					for _, q := range qs {
						if q.Asked.Question == "escalate-"+boundary {
							return q.State == trace.QuestionAnswered && q.Ruling != nil && q.Ruling.Revision == 2 && q.Ruling.OwnerResponse == "Keep the agreed contract." && resumed(repo, q)
						}
					}
					return false
				})
				_, err = f.C.Statuses(ctx)
				fixturekit.Must(t, err)
				otherStatus, err := f.C.Status(ctx, unrelated)
				fixturekit.Must(t, err)
				if otherStatus.Archived {
					t.Fatal("member retirement archived unrelated work")
				}
				otherSent, err := f.C.Send(ctx, unrelated, "Report independent status at "+boundary)
				fixturekit.Must(t, err)
				if boundary != "retained" {
					journey.LifecycleAwait(t, f, "independent owner turn "+boundary, func() bool {
						conversation, err := f.C.Conversation(ctx, unrelated)
						fixturekit.Must(t, err)
						return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
							return e.Turn == otherSent.Turn && e.Kind == "response" && e.State == api.TurnDone
						})
					})
				}
				otherRepo, err := f.S.Repository(f.Project)
				fixturekit.Must(t, err)
				continuedOther, err := otherRepo.ChiefOfStaffThread(unrelated)
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(otherChief.Identity, continuedOther.Identity) {
					t.Fatal("retirement replaced an unrelated chief")
				}
				archived, err := f.C.Status(ctx, retired)
				fixturekit.Must(t, err)
				if !archived.Archived {
					t.Fatal("archived status lost its lifecycle")
				}
			}
			check("retained")
			cleanupGate.Release()
			journey.LifecycleAwait(t, f, "production cleanup", func() bool {
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, retired)
				return ok && !a.CleanedAt.IsZero()
			})
			for _, r := range []*trace.Repository{first, second} {
				if _, err := r.Workflow(retired, trace.FeatureSubject); !errors.Is(err, trace.ErrMemberRetired) {
					t.Fatalf("stale handle bypassed retirement: %v", err)
				}
			}
			check("deleted")
			closeFirst()
			closeFirst = func() {}
			closeSecond()
			closeSecond = func() {}
			f.Stop(t)
			f.Start(t)
			check("restarted")
			gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, retiredProject, retired)
			fixturekit.Must(t, err)
			if !gone {
				t.Fatal("a shared open revived deleted history")
			}
			_, err = f.C.Abandon(ctx, survivor, "The remaining member is cancelled.")
			fixturekit.Must(t, err)
			journey.LifecycleAwait(t, f, "final group resolution", func() bool {
				r, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
				fixturekit.Must(t, err)
				defer r.Close()
				ids, rev, err := trace.ResolvedWorkgroups(r)
				fixturekit.Must(t, err)
				return slices.Equal(ids, []config.WorkgroupID{group.ID}) && rev == 1
			})
			f.Stop(t)
			f.Start(t)
			authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
			fixturekit.Must(t, err)
			ids, rev, err := trace.ResolvedWorkgroups(authority)
			fixturekit.Must(t, err)
			active, err := trace.ProjectSpecActive(authority, group.ID)
			fixturekit.Must(t, err)
			fixturekit.Must(t, authority.Close())
			if !slices.Equal(ids, []config.WorkgroupID{group.ID}) || rev != 1 || active {
				t.Fatal("restart duplicated resolution or retained active group documents")
			}

		})
	}
}
func TestDesignProductionLifecycleRejectsUnprovedRetirement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _ := newLifecycleDesignFixture(t)
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	_, projects := f.S.Runtimes()
	for _, p := range projects {
		fixturekit.Must(t, f.S.Halt(p))
	}
	repo, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	authority, _, _, release, err := f.S.Lifecycle().ChiefRepository(ctx, repo, f.Stream2)
	fixturekit.Must(t, err)
	defer release()
	path, err := f.S.Current().Root.WorkstreamTrace(f.Project, f.Stream1)
	fixturekit.Must(t, err)
	reject := func(t *testing.T) {
		t.Helper()
		if _, err := f.C.Conversation(ctx, f.Stream2); err == nil {
			t.Fatal("conversation masked invalid live binding")
		}
		if _, err := authority.Workflow(f.Stream1, trace.FeatureSubject); err == nil || errors.Is(err, trace.ErrMemberRetired) {
			t.Fatalf("live error interpreted as retirement: %v", err)
		}
		if retired, err := authority.MemberRetired(f.Stream1); retired {
			t.Fatalf("unproved member retirement: %v", err)
		}
	}
	t.Run("unexpectedly-missing", func(t *testing.T) {
		moved := path + ".unexplained"
		fixturekit.Must(t, os.Rename(path, moved))
		defer func() { fixturekit.Must(t, os.Rename(moved, path)) }()
		reject(t)
	})
	t.Run("corrupt-live", func(t *testing.T) {
		manifest := "workstreams/" + string(f.Stream1) + "/workstream.json"
		original, ok := fixturekit.TraceFile(t, path, manifest)
		if !ok {
			t.Fatal("the live member has no manifest")
		}
		fixturekit.TraceExec(t, path, "UPDATE files SET content = ? WHERE path = ?", []byte("corrupt live manifest\n"), manifest)
		defer fixturekit.TraceExec(t, path, "UPDATE files SET content = ? WHERE path = ?", original, manifest)
		reject(t)
	})
	t.Run("inactive-repo", func(t *testing.T) {
		cfg := f.S.Current()
		changed := *cfg
		changed.Projects = slices.DeleteFunc(slices.Clone(cfg.Projects), func(p config.Project) bool { return p.ID == f.Project })
		f.S.Mu.Lock()
		f.S.Cfg = &changed
		f.S.Mu.Unlock()
		defer func() { f.S.Mu.Lock(); f.S.Cfg = cfg; f.S.Mu.Unlock() }()
		reject(t)
	})
	t.Run("misconfigured-repo", func(t *testing.T) {
		_, active := f.S.RuntimeOf(f.Project)
		original := active.Repository
		f.S.Mu.Lock()
		active.Repository = repo
		f.S.Mu.Unlock()
		defer func() { f.S.Mu.Lock(); active.Repository = original; f.S.Mu.Unlock() }()
		if _, err := f.C.Conversation(ctx, f.Stream2); err == nil {
			t.Fatal("misconfigured repo resolved to a different member authority")
		}
	})
	t.Run("incomplete-join", func(t *testing.T) {
		groups, _, err := trace.Workgroups(authority)
		fixturekit.Must(t, err)
		incomplete := groups[0]
		incomplete.Members = slices.Clone(incomplete.Members[:2])
		members, err := f.S.Lifecycle().CrossRepoMemberRepos(ctx, incomplete)
		if err == nil {
			t.Fatalf("incomplete membership accepted: %+v", members)
		}
		if errors.Is(err, trace.ErrMemberRetired) {
			t.Fatal("incomplete join proved terminal state")
		}
	})
	t.Run("archive-without-cleanup-intent", func(t *testing.T) {

		_, err = f.C.Abandon(ctx, f.Stream1, "Cancelled by the owner.")
		fixturekit.Must(t, err)
		_, err = f.C.Archive(ctx, f.Stream1)
		fixturekit.Must(t, err)
		moved := path + ".unexplained"
		fixturekit.Must(t, os.Rename(path, moved))
		defer func() { fixturekit.Must(t, os.Rename(moved, path)) }()
		if _, err := f.C.Conversation(ctx, f.Stream2); err == nil {
			t.Fatal("archive masked unexplained disappearance")
		}
		if retired, err := authority.MemberRetired(f.Stream1); retired || err == nil {
			t.Fatalf("absence without cleanup intent proved retirement: retired=%v err=%v", retired, err)
		}
	})
	t.Run("retired-identity-mismatch", func(t *testing.T) {
		_, active := f.S.RuntimeOf(f.Project)
		original := active.Repository
		f.S.Mu.Lock()
		active.Repository = repo
		f.S.Mu.Unlock()
		defer func() { f.S.Mu.Lock(); active.Repository = original; f.S.Mu.Unlock() }()
		if _, err := f.C.Conversation(ctx, f.Stream2); err == nil {
			t.Fatal("retired binding masked a mismatched repository identity")
		}
	})
}
func TestDesignProductionLifecycleArchiveRecoveryAtBoundaries(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"member-retirement-snapshot", "member-archive-recorded", "archive-cleanup-ready", "archive-cleanup-intent", "archive-cleanup-renamed", "archive-cleanup-completed"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, cleanupGate := newLifecycleDesignFixture(t)
			defer func() { f.Stop(t) }()
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			f.AwaitApproved(t)
			repo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			id, err := repo.CrossRepoWorkgroupOf(f.Stream1)
			fixturekit.Must(t, err)
			authority, host, _, release, err := f.S.Lifecycle().ChiefRepository(ctx, repo, f.Stream1)
			fixturekit.Must(t, err)
			approval, pins, found, err := trace.ReadGroupApproval(authority, host)
			fixturekit.Must(t, err)
			release()
			if !found || !approval.Complete {
				t.Fatal("production seals are incomplete")
			}

			_, err = f.C.Abandon(ctx, f.Stream1, "Owner cancelled the source-repo member.")
			fixturekit.Must(t, err)
			hook := &intakefixture.DesignInterruption{Step: step, Nth: 1, Reached: make(chan struct{})}
			finished := make(chan error, 1)
			if strings.HasPrefix(step, "archive-cleanup-") {
				f.Hook.Store(hook)
				_, err = f.C.Archive(ctx, f.Stream1)
				fixturekit.Must(t, err)
				cleanupGate.Release()
			} else {
				f.Hook.Store(hook)
				go func() { _, err := f.C.Archive(ctx, f.Stream1); finished <- err }()
			}
			select {
			case <-hook.Reached:
			case err := <-finished:
				t.Fatalf("archive returned before retirement boundary: %v", err)
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("live controller did not reach retirement boundary")
			}
			f.Stop(t)
			if !strings.HasPrefix(step, "archive-cleanup-") {
				select {
				case err := <-finished:
					if err == nil {
						t.Fatal("interrupted archive reported success")
					}
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("archive request did not finish on shutdown")
				}
			}
			f.Hook.Store(nil)
			cleanupGate.Release()
			f.Start(t)
			if !strings.HasPrefix(step, "archive-cleanup-") {
				_, err = f.C.Archive(ctx, f.Stream1)
				fixturekit.Must(t, err)
				_, err = f.S.Repository(f.Project)
				fixturekit.Must(t, err)
				cleanupGate.Release()
			}
			journey.LifecycleAwait(t, f, "recovered production cleanup", func() bool {
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
				return ok && a.Workgroup == id && a.Resolved && !a.CleanupStartedAt.IsZero() && !a.CleanedAt.IsZero()
			})
			survivor, err := f.S.Repository(f.Project2)
			fixturekit.Must(t, err)
			target, actualHost, _, closeTarget, err := f.S.Lifecycle().ChiefRepository(ctx, survivor, f.Stream2)
			fixturekit.Must(t, err)
			got, doc, found, err := trace.ReadGroupApproval(target, actualHost)
			fixturekit.Must(t, err)
			active, err := trace.ProjectSpecActive(target, id)
			fixturekit.Must(t, err)
			closeTarget()
			if actualHost != host || !active || !found || !intakefixture.EqualJSONTest(got, approval) || !intakefixture.EqualJSONTest(doc, pins) {
				t.Fatal("interrupted retirement changed original authority, active contracts or approval pins")
			}
			sent, err := f.C.Send(ctx, f.Stream2, "Continue after interrupted retirement.")
			fixturekit.Must(t, err)
			journey.LifecycleAwait(t, f, "recovered shared chief owner turn", func() bool {
				conversation, err := f.C.Conversation(ctx, f.Stream2)
				fixturekit.Must(t, err)
				return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
					return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone
				})
			})
		})
	}
}

func TestDesignProductionMemberArchivedBeforeApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, cleanupGate := newLifecycleDesignFixture(t)
	defer func() { f.Stop(t) }()
	f.StartOwner(t)
	f.AwaitPacket(t)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	change, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "cancel-unapproved", Kind: "cancel", Scope: "workgroup", Change: "Cancel the whole workgroup before ratification"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	wt, g, _, closeGroup, err := f.S.Lifecycle().ChangeContext(ctx, string(f.Origin))
	fixturekit.Must(t, err)
	current := func(state string) func() bool {
		return func() bool {
			fixturekit.Must(t, f.S.Lifecycle().AdvanceChanges(ctx, wt, g))
			changes, err := lifecycle.ReadChanges(wt, g.Members[0])
			fixturekit.Must(t, err)
			for _, recorded := range changes {
				if recorded.ID == change.ID {
					change = recorded
				}
			}
			return change.State == state
		}
	}
	journey.LifecycleAwait(t, f, "cancellation presented", current("presented"))
	change, apiFault = f.S.Lifecycle().DecideChange(ctx, string(f.Origin), change.ID, lifecycle.ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "close"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	journey.LifecycleAwait(t, f, "cancellation applied", current("applied"))
	closeGroup()
	_, err = f.C.Archive(ctx, f.Stream1)
	fixturekit.Must(t, err)
	cleanupGate.Await(t, f)
	cleanupGate.Release()
	journey.LifecycleAwait(t, f, "unapproved member cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
		return ok && a.Workgroup == group && a.Resolved && !a.CleanedAt.IsZero()
	})
	readable := func(t *testing.T) {
		t.Helper()
		survivor, err := f.S.Repository(f.Project2)
		fixturekit.Must(t, err)
		_, g, found, err := survivor.CrossRepoProjectSpec(group)
		fixturekit.Must(t, err)
		if !found {
			t.Fatal("the workgroup trace is missing")
		}
		members, err := f.S.Lifecycle().CrossRepoMemberRepos(ctx, g)
		fixturekit.Must(t, err)
		if len(members) != 2 || !slices.ContainsFunc(members, func(m trace.CrossRepoMember) bool {
			return m.Workstream == f.Stream1 && m.Project.Project() == f.Project
		}) {
			t.Fatalf("the archived member is not bound to its project: %+v", members)
		}
		_, err = f.C.Inbox(ctx)
		fixturekit.Must(t, err)
		_, err = f.C.Conversation(ctx, f.Stream2)
		fixturekit.Must(t, err)
	}
	readable(t)
	f.Stop(t)
	f.Start(t)
	readable(t)
}
