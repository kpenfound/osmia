package service

import (
	"context"
	"fmt"
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

func TestDesignProductionLifecycleResolverAcrossArchiveCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	reached, resume, resumed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var readStep atomic.Pointer[string]
	var held atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	defer release()
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{
		wait: lifecycleAwait,
		checkpoint: func(lifetime context.Context, step string) error {
			if err := cleanup.checkpoint(lifetime, step); err != nil {
				return err
			}
			want := readStep.Load()
			if want == nil || step != *want || !held.CompareAndSwap(false, true) {
				return nil
			}
			close(reached)
			select {
			case <-resume:
				close(resumed)
				return nil
			case <-lifetime.Done():
				return lifetime.Err()
			}
		},
	})
	defer func() { release(); cleanup.release(); f.stop(t) }()
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	right, err := f.s.repository(f.project2)
	must(t, err)
	target, host, _, closeTarget, err := f.s.chiefRepository(ctx, right, f.stream2)
	must(t, err)
	defer func() { closeTarget() }()
	groups, _, err := trace.Workgroups(target)
	must(t, err)
	if len(groups) != 1 || !slices.Equal(groups[0].Members[1:], []config.WorkstreamID{f.stream1, f.stream2}) {
		t.Fatalf("source must precede unresolved survivor in recorded membership: %+v", groups)
	}
	group := groups[0]
	approval, approvalDoc, found, err := trace.ReadGroupApproval(target, host)
	must(t, err)
	if !found || !approval.Complete {
		t.Fatal("production group seals are not effective")
	}
	f.engine.mu.Lock()
	f.engine.turns["resolver-survivor-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "survivor-after-resolver-cleanup"})
		return questionResult(req, "session-worker", "Asked the original chief."), err
	}
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil || !slices.Contains(names, status.ToolName) {
			return questionResult(req, "session-worker", "Continued the original asking turn."), err
		}
		right, err := f.s.repository(f.project2)
		if err != nil {
			return nil, err
		}
		qs, err := right.Questions(f.stream2)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			if q.State == trace.QuestionOpen && q.Asked.Question == "survivor-after-resolver-cleanup" {
				_, err = callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.stream2) + ":" + q.Asked.ID, "text": "Keep the acknowledged offset.", "citations": []string{"spec#1"}})
				if err != nil {
					return nil, err
				}
			}
		}
		_, err = callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Keep the contract.", "note": "Shared authority remains available.", "agents": []string{}})
		return questionResult(req, "session-chief", "Read original authority."), err
	}
	f.engine.mu.Unlock()
	_, err = f.c.Abandon(ctx, f.stream1, "The owner cancelled the source member.")
	must(t, err)
	step := fmt.Sprintf("workgroup-member-resolution-read:%s:%s", f.project2, f.stream1)
	readStep.Store(&step)
	select {
	case <-reached:
	case <-time.After(demoTimeout):
		f.timeout(t, "survivor controller's source resolution read")
	}
	retired, err := target.MemberRetired(f.stream1)
	must(t, err)
	if retired {
		t.Fatal("resolver checkpoint did not hold a live binding")
	}
	_, err = f.c.Archive(ctx, f.stream1)
	must(t, err)
	cleanup.await(t)
	cleanup.release()
	lifecycleAwait(t, f, "source cleanup while survivor resolver is held", func() bool {
		a, ok := archiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	must(t, err)
	if !gone {
		t.Fatal("production cleanup did not remove the source history")
	}
	release()
	select {
	case <-resumed:
	case <-time.After(demoTimeout):
		t.Fatal("held production resolver did not resume")
	}
	check := func(boundary string) {
		t.Helper()
		right, err := f.s.repository(f.project2)
		must(t, err)
		current, currentHost, _, closeCurrent, err := f.s.chiefRepository(ctx, right, f.stream2)
		must(t, err)
		defer closeCurrent()
		ids, rev, err := trace.ResolvedWorkgroups(current)
		must(t, err)
		active, err := trace.ProjectSpecActive(current, group.ID)
		must(t, err)
		if len(ids) != 0 || rev != 0 || !active || currentHost != host {
			t.Fatal("retirement resolved the group or replaced the original authority")
		}
		got, doc, found, err := trace.ReadGroupApproval(current, host)
		must(t, err)
		if !found || !equalJSONTest(got, approval) || !equalJSONTest(doc, approvalDoc) {
			t.Fatal("resolver transition changed effective approval pins")
		}
		chief, err := current.Thread(host, trace.ChiefOfStaff)
		must(t, err)
		if !equalJSONTest(chief.Identity, f.before.Identity) || !equalJSONTest(chief.Turns[:len(f.before.Turns)], f.before.Turns) {
			t.Fatal("resolver transition replaced adopted conversation history")
		}
		spec, _, _, err := right.CrossRepoProjectSpec(group.ID)
		must(t, err)
		if len(spec.Contracts) == 0 {
			t.Fatal("survivor lost active contracts")
		}
		var feed FeedResponse
		must(t, f.c.Do(ctx, "GET", Prefix+"/feed/"+string(f.stream2), nil, &feed))
		sent, err := f.c.Send(ctx, f.stream2, "Report survivor status after resolver "+boundary)
		must(t, err)
		lifecycleAwait(t, f, "shared owner Send "+boundary, func() bool {
			conversation, err := f.c.Conversation(ctx, f.stream2)
			must(t, err)
			return slices.ContainsFunc(conversation.Entries, func(e ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == TurnDone
			})
		})
	}
	check("deleted")
	lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "resolver-survivor-ask", "survivor-after-resolver-cleanup")
	lifecycleAwait(t, f, "survivor answer and event after resolver retirement", func() bool {
		qs, err := right.Questions(f.stream2)
		must(t, err)
		if !slices.ContainsFunc(qs, func(q trace.QuestionState) bool {
			return q.Asked.Question == "survivor-after-resolver-cleanup" && q.State == trace.QuestionAnswered
		}) {
			return false
		}
		outbox, err := right.Outbox(f.stream2)
		must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		must(t, err)
		for _, e := range outbox {
			if e.Event.Operation != nil || !strings.Contains(e.Event.Body, "survivor-after-resolver-cleanup") {
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
				t.Fatalf("survivor event duplicated or lost: acks=%d deliveries=%d", acks, deliveries)
			}
			return true
		}
		return false
	})
	must(t, target.Close())
	closeTarget = func() {}
	f.stop(t)
	f.start(t)
	check("restarted")
	_, err = f.c.Abandon(ctx, f.stream2, "The remaining member is cancelled.")
	must(t, err)
	lifecycleAwait(t, f, "exactly once final resolution", func() bool {
		r, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group.ID)
		must(t, err)
		defer r.Close()
		ids, rev, err := trace.ResolvedWorkgroups(r)
		must(t, err)
		return slices.Equal(ids, []config.WorkgroupID{group.ID}) && rev == 1
	})
	f.stop(t)
	f.start(t)
	r, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group.ID)
	must(t, err)
	defer r.Close()
	ids, rev, err := trace.ResolvedWorkgroups(r)
	must(t, err)
	active, err := trace.ProjectSpecActive(r, group.ID)
	must(t, err)
	if !slices.Equal(ids, []config.WorkgroupID{group.ID}) || rev != 1 || active {
		t.Fatal("restart duplicated resolution or retained active group documents")
	}
}
