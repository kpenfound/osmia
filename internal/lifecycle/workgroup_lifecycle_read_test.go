package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionLifecycleReadAcrossArchiveCleanup(t *testing.T) {
	for _, read := range []string{"inbox", "ruling"} {
		t.Run(read, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f, cleanup := newLifecycleDesignFixture(t)
			defer func() { f.stop(t) }()
			f.startOwner(t)
			packet := f.awaitPacket(t)
			_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			must(t, err)
			f.awaitApproved(t)
			var escalated atomic.Bool
			f.engine.Mu.Lock()
			f.engine.Turns["lifecycle-read-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": req.Prompt})
				return questionResult(req, "session-worker", "Asked the shared chief."), err
			}
			f.engine.Turns["lifecycle-read-next"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "survivor-after-read-cleanup"})
				return questionResult(req, "session-worker", "Asked the shared chief."), err
			}
			f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				names, err := toolNames(ctx, tools)
				if err != nil || !slices.Contains(names, status.ToolName) {
					return questionResult(req, "session-worker", "Continued the original asking turn."), err
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
					leftRepo, err := f.s.repository(f.project)
					if err != nil {
						return nil, err
					}
					left, err := leftRepo.Questions(f.stream1)
					if err != nil {
						return nil, err
					}
					if len(left) == 1 && len(right) == 1 && escalated.CompareAndSwap(false, true) {
						_, err = callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.stream1) + ":1", string(f.stream2) + ":1"}, "rephrasing": "One original decision held across cleanup.", "blocked": "Both original asking turns.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
						if err != nil {
							return nil, err
						}
					}
				}
				for _, q := range right {
					if q.State == trace.QuestionOpen && q.Asked.Question == "survivor-after-read-cleanup" {
						_, err := callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.stream2) + ":" + q.Asked.ID, "text": "Keep the acknowledged offset.", "citations": []string{"spec#1"}})
						if err != nil {
							return nil, err
						}
					}
				}
				_, err = callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Keep the contract.", "note": "Original authority remains available.", "agents": []string{}})
				return questionResult(req, "session-chief", "Read original authority."), err
			}
			f.engine.Mu.Unlock()
			lifecycleWorker(t, f, f.project, f.stream1, "worker_first", "lifecycle-read-ask", "first-read-origin")
			lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "lifecycle-read-ask", "second-read-origin")
			rightRepo, err := f.s.repository(f.project2)
			must(t, err)
			target, host, _, releaseTarget, err := f.s.chiefRepository(ctx, rightRepo, f.stream2)
			must(t, err)
			defer releaseTarget()
			var original trace.InboxEntry
			lifecycleAwait(t, f, "original read batch", func() bool {
				entries, err := target.Inbox()
				must(t, err)
				if len(entries) == 1 && len(entries[0].Questions) == 2 && entries[0].Workstream == f.stream1 && entries[0].Hold == "" {
					for _, q := range entries[0].Questions {
						if q.Asked.ID == "" {
							return false
						}
					}
					original = entries[0]
					return true
				}
				return false
			})
			_, err = f.c.Answer(ctx, original.Number, "Keep the original contract.", target.Project())
			must(t, err)
			_, err = f.c.Abandon(ctx, f.stream1, "Owner cancelled the source member.")
			must(t, err)
			// Settle cancellation retries and eligible member notices before arming
			// the read checkpoint, where the injected clock stops advancing.
			// Cancellation keeps the source's remaining events undelivered.
			lifecycleAwait(t, f, "settled source retirement", func() bool {
				source, err := f.s.repository(f.project)
				must(t, err)
				sourceBusy, sourceChief, err := (&workspaceCleanup{repository: source}).busy(f.stream1)
				must(t, err)
				if len(sourceBusy) != 0 || sourceChief {
					return false
				}
				operations, err := source.Operations(f.stream1)
				must(t, err)
				for _, op := range operations {
					if op.Result == nil || !op.Acknowledged {
						return false
					}
				}
				for _, member := range []struct {
					project config.ProjectID
					stream  config.WorkstreamID
				}{{f.project, f.stream1}, {f.project2, f.stream2}} {
					repo, err := f.s.repository(member.project)
					must(t, err)
					skipped, err := chiefOfStaffSkip(repo)(member.stream)
					must(t, err)
					if skipped {
						continue
					}
					outbox, err := repo.Outbox(member.stream)
					must(t, err)
					for _, e := range outbox {
						if e.Event.Operation == nil && !e.Acknowledged {
							return false
						}
					}
				}
				chief, err := target.Thread(host, trace.ChiefOfStaff)
				must(t, err)
				return !slices.ContainsFunc(chief.Turns, func(turn trace.QueuedTurn) bool { return turn.CompletedAt.IsZero() })
			})
			before, err := target.Inbox()
			must(t, err)
			if len(before) != 1 || before[0].State != trace.QuestionRuled || before[0].Questions[0].Ruling == nil {
				t.Fatalf("original owner ruling was not recorded: %+v", before)
			}
			reached, resume := make(chan struct{}), make(chan struct{})
			var armed atomic.Bool
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			defer release()
			defer func() { must(t, target.SetQuestionReadBoundary(nil)) }()
			must(t, target.SetQuestionReadBoundary(func(stream config.WorkstreamID) error {
				if stream != f.stream1 || !armed.CompareAndSwap(false, true) {
					return nil
				}
				close(reached)
				select {
				case <-resume:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}))
			finished := make(chan error, 1)
			var entries []trace.InboxEntry
			var ruling trace.Ruling
			go func() {
				var err error
				if read == "inbox" {
					entries, err = target.Inbox()
				} else {
					ruling, err = target.QuestionRuling(f.stream1, "1", 1)
				}
				finished <- err
			}()
			select {
			case <-reached:
			case <-time.After(demoTimeout):
				t.Fatal("read did not resolve the live child")
			}
			select {
			case err := <-finished:
				t.Fatalf("read bypassed its resolved-child checkpoint: %v", err)
			default:
			}
			_, err = f.c.Archive(ctx, f.stream1)
			must(t, err)
			select {
			case <-cleanup.reached:
			case <-time.After(time.Minute):
				source, err := f.s.repository(f.project)
				must(t, err)
				busy, chief, err := (&workspaceCleanup{repository: source}).busy(f.stream1)
				t.Logf("source cleanup busy=%v chief=%v error=%v", busy, chief, err)
				for _, scope := range []struct {
					repo   *trace.Repository
					stream config.WorkstreamID
				}{{source, f.stream1}, {target, host}} {
					operations, err := scope.repo.Operations(scope.stream)
					must(t, err)
					for _, op := range operations {
						if op.Result == nil || !op.Acknowledged {
							t.Logf("cleanup scope project=%s workstream=%s operation=%s action=%s input=%s claim=%+v effect=%v result=%+v ack=%v retry=%v history=%+v", scope.repo.Project(), scope.stream, op.Operation.ID, op.Operation.Action, op.Operation.Input, op.Claim, op.EffectStarted, op.Result, op.Acknowledged, op.RetryAt, op.History)
						}
					}
					f.logTurns(t, scope.repo, scope.stream)
				}
				threads, err := source.Threads(f.stream1)
				must(t, err)
				for _, thread := range threads {
					for _, turn := range thread.Turns {
						if turn.CompletedAt.IsZero() || turn.Request.TurnID == thread.Active {
							t.Logf("unfinished source thread=%s role=%s active=%s turn=%s cause=%s status=%s queued=%v claim=%+v response=%+v attempts=%+v", thread.Identity.ID, thread.Identity.Role, thread.Active, turn.Request.TurnID, turn.Request.Cause, turn.Status(), turn.Request.At, turn.Claim, turn.Response, turn.Attempts)
						}
					}
				}
				f.timeout(t, "eligible source cleanup after held read")
			}
			cleanup.release()
			lifecycleAwait(t, f, "closed and removed source child", func() bool { a, ok := archiveRecord(t, f.s.store, f.stream1); return ok && !a.CleanedAt.IsZero() })
			gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
			must(t, err)
			if !gone {
				t.Fatal("production cleanup did not remove the resolved child")
			}
			release()
			select {
			case err := <-finished:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("resolved-child read did not resume")
			}
			if read == "inbox" {
				if len(entries) != 1 || entries[0].Number != original.Number || entries[0].Workstream != f.stream1 || entries[0].Batch != original.Batch || !equalJSONTest(entries[0].Questions, before[0].Questions) || entries[0].Hold == "" || entries[0].State != trace.QuestionRuled {
					t.Fatalf("read lost original pending authority: %+v", entries)
				}
			} else if !equalJSONTest(ruling, *before[0].Questions[0].Ruling) {
				t.Fatalf("read changed the exact original ruling: %+v", ruling)
			}
			_, _, err = target.QuestionActions(f.stream1)
			if !errors.Is(err, trace.ErrMemberRetired) {
				t.Fatalf("retired member actions did not retain their recoverable hold: %v", err)
			}
			lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "lifecycle-read-next", "survivor-after-read-cleanup")
			lifecycleAwait(t, f, "survivor event and answer after held read", func() bool {
				qs, err := rightRepo.Questions(f.stream2)
				must(t, err)
				if !slices.ContainsFunc(qs, func(q trace.QuestionState) bool {
					return q.Asked.Question == "survivor-after-read-cleanup" && q.State == trace.QuestionAnswered
				}) {
					return false
				}
				outbox, err := rightRepo.Outbox(f.stream2)
				must(t, err)
				chief, err := target.Thread(host, trace.ChiefOfStaff)
				must(t, err)
				for _, e := range outbox {
					if e.Event.Operation != nil || !strings.Contains(e.Event.Body, "survivor-after-read-cleanup") {
						continue
					}
					if !e.Acknowledged {
						return false
					}
					acks, deliveries := 0, 0
					claims := map[string]bool{}
					for _, action := range e.History {
						if action.Kind == "acknowledge" {
							acks++
						}
						if action.Kind == "claim" {
							claims[events.TurnID(action.Token)] = true
						}
					}
					for _, turn := range chief.Turns {
						if claims[turn.Request.TurnID] && !turn.CompletedAt.IsZero() && turn.Status() == "idle" {
							deliveries++
						}
					}
					if acks != 1 || deliveries != 1 {
						t.Fatalf("survivor event lost original once-only delivery: acks=%d deliveries=%d", acks, deliveries)
					}
					return true
				}
				return false
			})
		})
	}
}
