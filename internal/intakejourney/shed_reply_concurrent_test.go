package intakejourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAcceptedReplyRedraftDuringPassOpensTheNextRound(t *testing.T) {
	t.Parallel()
	var armed atomic.Bool
	read, resume, correct := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var resumeOnce, correctOnce sync.Once
	releasePass := func() { resumeOnce.Do(func() { close(resume) }) }
	releaseReply := func() { correctOnce.Do(func() { close(correct) }) }
	f := journey.NewDebateFixtureWith(t, 1, 2, "", func(o *journey.Options) {
		o.Boundary = func(ctx context.Context, step string) error {
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
	defer f.Stop(t)
	defer releasePass()
	defer releaseReply()
	p := &intakefixture.Faults{}
	defer p.Check(t)
	proof := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Acceptance, "plan#dedupe", "spec#2"))
	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: fixturekit.CyclicPlan}, intakefixture.Answers(p, "A test shows it now.", proof))
	f.Script(intake.ReplyTurnID(1, 2), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		armed.Store(true)
		select {
		case <-correct:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	f.Member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if got, err := intakefixture.ReadTool(ctx, tools, "plan.json"); err != nil || got != splitPlan {
			p.Report("round 2 debates %q %v", got, err)
		}
		return intakefixture.Concedes(p, proof)(ctx, req, verified, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-read:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the pass never read the draft while the corrected turn was held")
	}
	releaseReply()
	f.AwaitShed(t, stream, "replied-1")
	reply := f.Reply(t, stream, 1)
	if reply.Turn != intake.ReplyTurnID(1, 2) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || reply.Failure != "" || len(reply.Problems) != 0 {
		t.Fatalf("accepted corrected reply: %+v", reply)
	}
	releasePass()
	f.AwaitShed(t, stream, "concluded-2", "concluded-1")
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 1 || ran[intake.ReplyTurnID(1, 2)] != 1 || ran[intake.ReplyTurnID(1, 3)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	if ops := f.AcknowledgedReplies(t, stream); len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("reply operations: %+v", ops)
	}
	if intakefixture.DocumentCommit(t, f.S.Current().Root, f.Project, stream, plan.PlanPath) != intakefixture.DocumentCommit(t, f.S.Current().Root, f.Project, stream, shed.ReplyPath(1)) {
		t.Fatal("the corrected redraft and reply landed in separate commits")
	}
}
