package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionLifecycleInterruptedRulingAndRelay(t *testing.T) {
	for _, stage := range []string{"owner-ruling-member", "chief-relay-member"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			f, cleanupGate := newLifecycleDesignFixture(t)
			defer func() { f.stop(t) }()
			f.startOwner(t)
			packet := f.awaitPacket(t)
			_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			must(t, err)
			f.awaitApproved(t)
			var escalated, relay atomic.Bool
			const text = "Keep the acknowledged offset contract."
			f.engine.mu.Lock()
			f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				names, err := toolNames(ctx, tools)
				if err != nil || !slices.Contains(names, status.ToolName) {
					return questionResult(req, "session-worker", "Continued the original asking turn."), err
				}
				leftRepo, err := f.s.repository(f.project)
				if err != nil {
					return nil, err
				}
				rightRepo, err := f.s.repository(f.project2)
				if err != nil {
					return nil, err
				}
				right, err := rightRepo.Questions(f.stream2)
				if err != nil {
					return nil, err
				}
				if !escalated.Load() {
					left, err := leftRepo.Questions(f.stream1)
					if err != nil {
						return nil, err
					}
					if len(left) == 1 && len(right) == 1 && escalated.CompareAndSwap(false, true) {
						_, err = callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.stream1) + ":1", string(f.stream2) + ":1"}, "rephrasing": "One contract decision for the original origins.", "blocked": "Both original asking turns.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
						if err != nil {
							return nil, err
						}
					}
				}
				if relay.Load() && len(right) == 1 && right[0].State == trace.QuestionRuled {
					_, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.stream1) + ":1", "text": text, "scope": "local"})
					// Sufficient survivor evidence is applied; an unfinished retired
					// relay remains explicitly held in the original group inbox.
					if err != nil && !strings.Contains(err.Error(), trace.ErrMemberRetired.Error()) {
						return nil, err
					}
				}
				_, err = callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Keep the contract.", "note": "Original authority and eligible origins remain active.", "agents": []string{}})
				return questionResult(req, "session-chief", "Original origins reconciled."), err
			}
			f.engine.turns["lifecycle-batch"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				question := "Second original contract question."
				if strings.Contains(req.SessionDir, "worker_first") {
					question = "First original contract question."
				}
				_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
				return questionResult(req, "session-worker", "Asked the shared chief."), err
			}
			f.engine.mu.Unlock()
			lifecycleWorker(t, f, f.project, f.stream1, "worker_first", "lifecycle-batch", "First original contract question.")
			lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "lifecycle-batch", "Second original contract question.")
			var original InboxEntry
			lifecycleAwait(t, f, "original consolidated inbox", func() bool {
				inbox, err := f.c.Inbox(ctx)
				must(t, err)
				for _, e := range inbox.Entries {
					if e.Question == "One contract decision for the original origins." {
						original = e
						return len(e.Asked) == 2 && e.Workstream == f.stream1
					}
				}
				return false
			})
			hook := &designInterruption{step: stage, nth: 1, reached: make(chan struct{})}
			f.hook.Store(hook)
			finished := make(chan error, 1)
			if stage == "chief-relay-member" {
				_, err = f.c.Answer(ctx, original.Number, text, original.Project)
				must(t, err)
				relay.Store(true)
				_, err = f.c.Send(ctx, f.stream2, "Relay the recorded owner decision.")
				must(t, err)
			} else {
				go func() { _, err := f.c.Answer(ctx, original.Number, text, original.Project); finished <- err }()
			}
			select {
			case <-hook.reached:
			case <-time.After(demoTimeout):
				t.Fatal("production question commit did not reach interruption")
			}
			f.stop(t)
			if stage == "owner-ruling-member" {
				select {
				case err := <-finished:
					if err == nil {
						t.Fatal("interrupted owner ruling reported completion")
					}
				case <-time.After(demoTimeout):
					t.Fatal("interrupted owner endpoint failed to finish")
				}
			}
			f.hook.Store(nil)
			relay.Store(false)
			f.start(t)
			retiring, err := f.s.repository(f.project)
			must(t, err)
			before, err := retiring.Questions(f.stream1)
			must(t, err)
			if len(before) != 1 || before[0].Ruling == nil {
				t.Fatal("first origin commit was not durable")
			}

			_, err = f.c.Abandon(ctx, f.stream1, "Owner cancelled the original first member.")
			must(t, err)
			_, err = f.c.Archive(ctx, f.stream1)
			must(t, err)
			cleanupGate.await(t)
			check := func(boundary string) {
				t.Helper()
				repo, err := f.s.repository(f.project2)
				must(t, err)
				target, host, _, release, err := f.s.chiefRepository(ctx, repo, f.stream2)
				must(t, err)
				defer release()
				inbox, err := target.Inbox()
				must(t, err)
				if len(inbox) != 1 || inbox[0].Number != original.Number || inbox[0].Batch != original.Batch || inbox[0].Workstream != f.stream1 || len(inbox[0].Questions) != 2 {
					t.Fatalf("%s lost original batch authority: %+v", boundary, inbox)
				}
				pinned, err := target.QuestionRuling(f.stream1, "1", before[0].Ruling.Revision)
				must(t, err)
				if !equalJSONTest(pinned, *before[0].Ruling) {
					t.Fatal("retirement changed original ruling attribution or links")
				}
				_, err = f.c.Answer(ctx, original.Number, "Change the contract instead.", original.Project)
				if err == nil {
					t.Fatal("different-text ruling retry succeeded")
				}
				if stage == "owner-ruling-member" && boundary == "retained" {
					_, err = f.c.Answer(ctx, original.Number, text, original.Project)
					must(t, err)
				}
				relay.Store(true)
				_, err = f.c.Send(ctx, f.stream2, "Continue original ruling at "+boundary)
				must(t, err)
				lifecycleAwait(t, f, "eligible original continuation "+boundary, func() bool {
					qs, err := repo.Questions(f.stream2)
					must(t, err)
					worker, err := repo.Thread(f.stream2, "worker_second")
					must(t, err)
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
				must(t, err)
				if stage == "owner-ruling-member" {
					if inbox[0].Hold == "" || inbox[0].State != trace.QuestionRuled || !equalJSONTest(inbox[0].Questions[0], before[0]) {
						t.Fatalf("unfinished retired relay falsely completed: %+v", inbox[0])
					}
					visible, err := f.c.Inbox(ctx)
					must(t, err)
					if !slices.ContainsFunc(visible.Entries, func(e InboxEntry) bool {
						return e.Project == original.Project && e.Number == original.Number && strings.Contains(e.Blocked, "operation remains held")
					}) {
						t.Fatal("held original entry disappeared from owner inbox")
					}
				} else if inbox[0].Hold != "" || inbox[0].State != trace.QuestionAnswered {
					t.Fatal("completed saved relay did not permit eligible survivor recovery")
				}
				_, err = target.Thread(host, trace.ChiefOfStaff)
				must(t, err)
			}
			check("retained")
			cleanupGate.release()
			lifecycleAwait(t, f, "interrupted-origin cleanup", func() bool { a, ok := archiveRecord(t, f.s.store, f.stream1); return ok && !a.CleanedAt.IsZero() })
			check("deleted")
			f.stop(t)
			f.start(t)
			check("restarted")
			gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
			must(t, err)
			if !gone {
				t.Fatal("original retired history was revived")
			}
			if _, err := f.c.Answer(ctx, original.Number, text, original.Project); err == nil || errors.Is(err, context.Canceled) {
				t.Fatal("completed owner ruling retry granted another effect")
			}
		})
	}
}
