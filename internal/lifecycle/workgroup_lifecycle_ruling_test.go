package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionLifecycleInterruptedRulingAndRelay(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"owner-ruling-member", "manager-relay-member"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			f, cleanupGate := newLifecycleDesignFixture(t)
			defer func() { f.Stop(t) }()
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			f.AwaitApproved(t)
			var escalated, relay atomic.Bool
			const text = "Keep the acknowledged offset contract."
			f.Engine.Mu.Lock()
			f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				names, err := intakefixture.ToolNames(ctx, tools)
				if err != nil || !slices.Contains(names, status.ToolName) {
					return intakefixture.QuestionResult(req, "session-worker", "Continued the original asking turn."), err
				}
				leftRepo, err := f.S.Repository(f.Project)
				if err != nil {
					return nil, err
				}
				rightRepo, err := f.S.Repository(f.Project2)
				if err != nil {
					return nil, err
				}
				right, err := rightRepo.Questions(f.Stream2)
				if err != nil {
					return nil, err
				}
				if !escalated.Load() {
					left, err := leftRepo.Questions(f.Stream1)
					if err != nil {
						return nil, err
					}
					if len(left) == 1 && len(right) == 1 && escalated.CompareAndSwap(false, true) {
						_, err = fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.Stream1) + ":1", string(f.Stream2) + ":1"}, "rephrasing": "One contract decision for the original origins.", "blocked": "Both original asking turns.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
						if err != nil {
							return nil, err
						}
					}
				}
				if relay.Load() && len(right) == 1 && right[0].State == trace.QuestionRuled {
					_, err := fixturekit.CallTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.Stream1) + ":1", "text": text, "scope": "local"})
					if err != nil && !strings.Contains(err.Error(), trace.ErrMemberRetired.Error()) {
						return nil, err
					}
				}
				_, err = fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Keep the contract.", "note": "Original authority and eligible origins remain active.", "agents": []string{}})
				return intakefixture.QuestionResult(req, "session-manager", "Original origins reconciled."), err
			}
			f.Engine.Turns["lifecycle-batch"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				question := "Second original contract question."
				if strings.Contains(req.SessionDir, "worker_first") {
					question = "First original contract question."
				}
				_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
				return intakefixture.QuestionResult(req, "session-worker", "Asked the shared manager."), err
			}
			f.Engine.Mu.Unlock()
			lifecycleWorker(t, f, f.Project, f.Stream1, "worker_first", "lifecycle-batch", "First original contract question.")
			lifecycleWorker(t, f, f.Project2, f.Stream2, "worker_second", "lifecycle-batch", "Second original contract question.")
			var original api.InboxEntry
			journey.LifecycleAwait(t, f, "original consolidated inbox", func() bool {
				inbox, err := f.C.Inbox(ctx)
				fixturekit.Must(t, err)
				for _, e := range inbox.Entries {
					if e.Question == "One contract decision for the original origins." {
						original = e
						return len(e.Asked) == 2 && e.Workstream == f.Stream1 && !slices.ContainsFunc(e.Asked, func(q api.InboxQuestion) bool { return q.ID == "" })
					}
				}
				return false
			})
			hook := &intakefixture.DesignInterruption{Step: stage, Nth: 1, Reached: make(chan struct{})}
			f.Hook.Store(hook)
			finished := make(chan error, 1)
			if stage == "manager-relay-member" {
				_, err = f.C.Answer(ctx, original.Number, text, original.Project)
				fixturekit.Must(t, err)
				relay.Store(true)
				_, err = f.C.Send(ctx, f.Stream2, "Relay the recorded owner decision.")
				fixturekit.Must(t, err)
			} else {
				go func() { _, err := f.C.Answer(ctx, original.Number, text, original.Project); finished <- err }()
			}
			select {
			case <-hook.Reached:
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("production question commit did not reach interruption")
			}
			f.Stop(t)
			if stage == "owner-ruling-member" {
				select {
				case err := <-finished:
					if err == nil {
						t.Fatal("interrupted owner ruling reported completion")
					}
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("interrupted owner endpoint failed to finish")
				}
			}
			f.Hook.Store(nil)
			relay.Store(false)
			f.Start(t)
			retiring, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			before, err := retiring.Questions(f.Stream1)
			fixturekit.Must(t, err)
			if len(before) != 1 || before[0].Ruling == nil {
				t.Fatal("first origin commit was not durable")
			}

			_, err = f.C.Abandon(ctx, f.Stream1, "Owner cancelled the original first member.")
			fixturekit.Must(t, err)
			_, err = f.C.Archive(ctx, f.Stream1)
			fixturekit.Must(t, err)
			cleanupGate.Await(t, f)
			check := func(boundary string) {
				t.Helper()
				repo, err := f.S.Repository(f.Project2)
				fixturekit.Must(t, err)
				target, host, _, release, err := f.S.Lifecycle().ManagerRepository(ctx, repo, f.Stream2)
				fixturekit.Must(t, err)
				defer release()
				inbox, err := target.Inbox()
				fixturekit.Must(t, err)
				if len(inbox) != 1 || inbox[0].Number != original.Number || inbox[0].Batch != original.Batch || inbox[0].Workstream != f.Stream1 || len(inbox[0].Questions) != 2 {
					t.Fatalf("%s lost original batch authority: %+v", boundary, inbox)
				}
				pinned, err := target.QuestionRuling(f.Stream1, "1", before[0].Ruling.Revision)
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(pinned, *before[0].Ruling) {
					t.Fatal("retirement changed original ruling attribution or links")
				}
				_, err = f.C.Answer(ctx, original.Number, "Change the contract instead.", original.Project)
				if err == nil {
					t.Fatal("different-text ruling retry succeeded")
				}
				if stage == "owner-ruling-member" && boundary == "retained" {
					_, err = f.C.Answer(ctx, original.Number, text, original.Project)
					fixturekit.Must(t, err)
				}
				relay.Store(true)
				_, err = f.C.Send(ctx, f.Stream2, "Continue original ruling at "+boundary)
				fixturekit.Must(t, err)
				journey.LifecycleAwait(t, f, "eligible original continuation "+boundary, func() bool {
					qs, err := repo.Questions(f.Stream2)
					fixturekit.Must(t, err)
					worker, err := repo.Thread(f.Stream2, "worker_second")
					fixturekit.Must(t, err)
					count, done := 0, false
					for _, turn := range worker.Turns {
						if turn.Request.TurnID == questions.TurnID("1") {
							count++
							done = turn.Response != nil && !turn.CompletedAt.IsZero() && turn.Status() == "idle"
						}
					}
					if count > 1 {
						t.Fatal("duplicate survivor continuation")
					}
					return len(qs) == 1 && qs[0].State == trace.QuestionAnswered && qs[0].Ruling.Revision == 2 && qs[0].Ruling.OwnerResponse == text && qs[0].Ruling.ReturnedAnswer == text && count == 1 && done
				})
				inbox, err = target.Inbox()
				fixturekit.Must(t, err)
				if stage == "owner-ruling-member" {
					if inbox[0].Hold == "" || inbox[0].State != trace.QuestionRuled || !intakefixture.EqualJSONTest(inbox[0].Questions[0], before[0]) {
						t.Fatalf("unfinished retired relay falsely completed: %+v", inbox[0])
					}
					visible, err := f.C.Inbox(ctx)
					fixturekit.Must(t, err)
					if !slices.ContainsFunc(visible.Entries, func(e api.InboxEntry) bool {
						return e.Project == original.Project && e.Number == original.Number && strings.Contains(e.Blocked, "operation remains held")
					}) {
						t.Fatal("held original entry disappeared from owner inbox")
					}
				} else if inbox[0].Hold != "" || inbox[0].State != trace.QuestionAnswered {
					t.Fatal("completed saved relay did not permit eligible survivor recovery")
				}
				_, err = target.Thread(host, trace.Manager)
				fixturekit.Must(t, err)
			}
			check("retained")
			cleanupGate.Release()
			journey.LifecycleAwait(t, f, "interrupted-origin cleanup", func() bool {
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
				return ok && !a.CleanedAt.IsZero()
			})
			check("deleted")
			f.Stop(t)
			f.Start(t)
			check("restarted")
			gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
			fixturekit.Must(t, err)
			if !gone {
				t.Fatal("original retired history was revived")
			}
			if _, err := f.C.Answer(ctx, original.Number, text, original.Project); err == nil || errors.Is(err, context.Canceled) {
				t.Fatal("completed owner ruling retry granted another effect")
			}
		})
	}
}
