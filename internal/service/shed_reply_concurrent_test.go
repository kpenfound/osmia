package service

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
)

// A reply can commit its corrected draft between a pass's initial draft read
// and its shed-state read. The next round must debate the accepted revision.
func TestAcceptedReplyRedraftDuringPassOpensTheNextRound(t *testing.T) {
	t.Parallel()
	var armed atomic.Bool
	read, resume, correct := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var resumeOnce, correctOnce sync.Once
	releasePass := func() { resumeOnce.Do(func() { close(resume) }) }
	releaseReply := func() { correctOnce.Do(func() { close(correct) }) }
	f := newDebateFixtureWith(t, 1, 2, "", func(o *Options) {
		o.boundary = func(ctx context.Context, step string) error {
			if step == "shed-draft-read" && armed.CompareAndSwap(true, false) {
				close(read)
				select {
				case <-resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
	})
	defer f.stop(t)
	defer releasePass()
	defer releaseReply()
	p := &faults{}
	defer p.check(t)
	proof := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Acceptance, "plan#dedupe", "spec#2"))
	f.script(replyTurnID(1, 1), map[string]string{plan.PlanPath: cyclicPlan}, answers(p, "A test shows it now.", proof))
	f.script(replyTurnID(1, 2), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		armed.Store(true)
		select {
		case <-correct:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	f.member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if got, err := readTool(ctx, tools, "plan.json"); err != nil || got != splitPlan {
			p.report("round 2 debates %q %v", got, err)
		}
		return concedes(p, proof)(ctx, req, verified, tools)
	})
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-read:
	case <-time.After(demoTimeout):
		t.Fatal("the pass never read the draft while the corrected turn was held")
	}
	releaseReply()
	f.awaitShed(t, stream, "replied-1")
	reply := f.reply(t, stream, 1)
	if reply.Turn != replyTurnID(1, 2) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || reply.Failure != "" || len(reply.Problems) != 0 {
		t.Fatalf("accepted corrected reply: %+v", reply)
	}
	releasePass()
	f.awaitShed(t, stream, "concluded-2", "concluded-1")
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.ran(); ran[replyTurnID(1, 1)] != 1 || ran[replyTurnID(1, 2)] != 1 || ran[replyTurnID(1, 3)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	if ops := f.acknowledgedReplies(t, stream); len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("reply operations: %+v", ops)
	}
	if documentCommit(t, f.s.cfg.Root, f.project, stream, plan.PlanPath) != documentCommit(t, f.s.cfg.Root, f.project, stream, shed.ReplyPath(1)) {
		t.Fatal("the corrected redraft and reply landed in separate commits")
	}
}
