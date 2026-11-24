package lifecycle_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionLifecycleResolverAcrossArchiveCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(cleanup.Release)
	reached, resume, resumed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var readStep atomic.Pointer[string]
	var held atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	defer release()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{
		Wait: journey.LifecycleAwait,
		Checkpoint: func(lifetime context.Context, step string) error {
			if err := cleanup.Checkpoint(lifetime, step); err != nil {
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
	defer func() { release(); cleanup.Release(); f.Stop(t) }()
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	right, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	target, host, _, closeTarget, err := f.S.Lifecycle().ChiefRepository(ctx, right, f.Stream2)
	fixturekit.Must(t, err)
	defer func() { closeTarget() }()
	groups, _, err := trace.Workgroups(target)
	fixturekit.Must(t, err)
	if len(groups) != 1 || !slices.Equal(groups[0].Members[1:], []config.WorkstreamID{f.Stream1, f.Stream2}) {
		t.Fatalf("source must precede unresolved survivor in recorded membership: %+v", groups)
	}
	group := groups[0]
	approval, approvalDoc, found, err := trace.ReadGroupApproval(target, host)
	fixturekit.Must(t, err)
	if !found || !approval.Complete {
		t.Fatal("production group seals are not effective")
	}
	f.Engine.Mu.Lock()
	f.Engine.Turns["resolver-survivor-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "survivor-after-resolver-cleanup"})
		return intakefixture.QuestionResult(req, "session-worker", "Asked the original chief."), err
	}
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil || !slices.Contains(names, status.ToolName) {
			return intakefixture.QuestionResult(req, "session-worker", "Continued the original asking turn."), err
		}
		right, err := f.S.Repository(f.Project2)
		if err != nil {
			return nil, err
		}
		qs, err := right.Questions(f.Stream2)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			if q.State == trace.QuestionOpen && q.Asked.Question == "survivor-after-resolver-cleanup" {
				_, err = fixturekit.CallTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.Stream2) + ":" + q.Asked.ID, "text": "Keep the acknowledged offset.", "citations": []string{"spec#1"}})
				if err != nil {
					return nil, err
				}
			}
		}
		_, err = fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Keep the contract.", "note": "Shared authority remains available.", "agents": []string{}})
		return intakefixture.QuestionResult(req, "session-chief", "Read original authority."), err
	}
	f.Engine.Mu.Unlock()
	_, err = f.C.Abandon(ctx, f.Stream1, "The owner cancelled the source member.")
	fixturekit.Must(t, err)
	step := fmt.Sprintf("workgroup-member-resolution-read:%s:%s", f.Project2, f.Stream1)
	readStep.Store(&step)
	select {
	case <-reached:
	case <-time.After(fixturekit.DemoTimeout):
		f.Timeout(t, "survivor controller's source resolution read")
	}
	retired, err := target.MemberRetired(f.Stream1)
	fixturekit.Must(t, err)
	if retired {
		t.Fatal("resolver checkpoint did not hold a live binding")
	}
	_, err = f.C.Archive(ctx, f.Stream1)
	fixturekit.Must(t, err)
	cleanup.Await(t)
	cleanup.Release()
	journey.LifecycleAwait(t, f, "source cleanup while survivor resolver is held", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("production cleanup did not remove the source history")
	}
	release()
	select {
	case <-resumed:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("held production resolver did not resume")
	}
	check := func(boundary string) {
		t.Helper()
		right, err := f.S.Repository(f.Project2)
		fixturekit.Must(t, err)
		current, currentHost, _, closeCurrent, err := f.S.Lifecycle().ChiefRepository(ctx, right, f.Stream2)
		fixturekit.Must(t, err)
		defer closeCurrent()
		ids, rev, err := trace.ResolvedWorkgroups(current)
		fixturekit.Must(t, err)
		active, err := trace.ProjectSpecActive(current, group.ID)
		fixturekit.Must(t, err)
		if len(ids) != 0 || rev != 0 || !active || currentHost != host {
			t.Fatal("retirement resolved the group or replaced the original authority")
		}
		got, doc, found, err := trace.ReadGroupApproval(current, host)
		fixturekit.Must(t, err)
		if !found || !intakefixture.EqualJSONTest(got, approval) || !intakefixture.EqualJSONTest(doc, approvalDoc) {
			t.Fatal("resolver transition changed effective approval pins")
		}
		chief, err := current.Thread(host, trace.ChiefOfStaff)
		fixturekit.Must(t, err)
		if !intakefixture.EqualJSONTest(chief.Identity, f.Before.Identity) || !intakefixture.EqualJSONTest(chief.Turns[:len(f.Before.Turns)], f.Before.Turns) {
			t.Fatal("resolver transition replaced adopted conversation history")
		}
		spec, _, _, err := right.CrossRepoProjectSpec(group.ID)
		fixturekit.Must(t, err)
		if len(spec.Contracts) == 0 {
			t.Fatal("survivor lost active contracts")
		}
		var feed api.FeedResponse
		fixturekit.Must(t, f.C.Do(ctx, "GET", api.Prefix+"/feed/"+string(f.Stream2), nil, &feed))
		sent, err := f.C.Send(ctx, f.Stream2, "Report survivor status after resolver "+boundary)
		fixturekit.Must(t, err)
		journey.LifecycleAwait(t, f, "shared owner Send "+boundary, func() bool {
			conversation, err := f.C.Conversation(ctx, f.Stream2)
			fixturekit.Must(t, err)
			return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone
			})
		})
	}
	check("deleted")
	lifecycleWorker(t, f, f.Project2, f.Stream2, "worker_second", "resolver-survivor-ask", "survivor-after-resolver-cleanup")
	journey.LifecycleAwait(t, f, "survivor answer and event after resolver retirement", func() bool {
		qs, err := right.Questions(f.Stream2)
		fixturekit.Must(t, err)
		if !slices.ContainsFunc(qs, func(q trace.QuestionState) bool {
			return q.Asked.Question == "survivor-after-resolver-cleanup" && q.State == trace.QuestionAnswered
		}) {
			return false
		}
		outbox, err := right.Outbox(f.Stream2)
		fixturekit.Must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		fixturekit.Must(t, err)
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
	fixturekit.Must(t, target.Close())
	closeTarget = func() {}
	f.Stop(t)
	f.Start(t)
	check("restarted")
	_, err = f.C.Abandon(ctx, f.Stream2, "The remaining member is cancelled.")
	fixturekit.Must(t, err)
	journey.LifecycleAwait(t, f, "exactly once final resolution", func() bool {
		r, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
		fixturekit.Must(t, err)
		defer r.Close()
		ids, rev, err := trace.ResolvedWorkgroups(r)
		fixturekit.Must(t, err)
		return slices.Equal(ids, []config.WorkgroupID{group.ID}) && rev == 1
	})
	f.Stop(t)
	f.Start(t)
	r, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
	fixturekit.Must(t, err)
	defer r.Close()
	ids, rev, err := trace.ResolvedWorkgroups(r)
	fixturekit.Must(t, err)
	active, err := trace.ProjectSpecActive(r, group.ID)
	fixturekit.Must(t, err)
	if !slices.Equal(ids, []config.WorkgroupID{group.ID}) || rev != 1 || active {
		t.Fatal("restart duplicated resolution or retained active group documents")
	}
}
