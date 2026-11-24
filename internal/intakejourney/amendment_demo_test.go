package intakejourney

import (
	context "context"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	slices "slices"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	seal "github.com/kpenfound/osmia/internal/seal"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAmendmentDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := journey.NewParallelMasonFixture(t, 2, 2, journey.ParallelPlan)
	defer f.Stop(t)
	f.Script("amend-1-1", map[string]string{plan.SpecPath: journey.AmendedSpec}, nil)
	f.Script("amend-1-round-1-"+intake.CommitteeAgent(1)+"-1", nil, nil)
	f.Script("amend-1-reply-1", nil, nil)
	for _, unit := range []string{"upload", "audit"} {
		masons.Play[execution.MasonTurnID(unit)] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
			recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Proof recorded"})
			if err != nil || !recorded {
				return fmt.Errorf("%s done: %s: %v", unit, reason, err)
			}
			return nil
		}
	}
	masons.Chief.Route("1", []string{"spec#1"}, "Resume from a durable checkpoint", "Acknowledgements are not durable")
	masons.Play[execution.MasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "Acknowledgements are not durable. Should the spec say a durable checkpoint?"})
		return err
	}
	stream, _ := f.BuiltAs(t, "amendment")
	f.AwaitUnit(t, stream, "resume", intake.UnitWaiting)
	f.AwaitMasonRan(t, stream, "audit")
	f.AwaitAmendment(t, stream, intake.AmendmentPresented)
	view, err := f.C.Amendment(ctx, stream, "1")
	fixturekit.Must(t, err)
	if view.Round != 1 || view.Revision != 1 || !strings.Contains(string(view.Packet), "durable checkpoint") {
		t.Fatalf("owner packet %+v", view)
	}
	before, _, _, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)

	f.Stop(t)
	f.Start(t)
	again, err := f.C.Amendment(ctx, stream, "1")
	fixturekit.Must(t, err)
	if again.Round != 1 || again.Revision != 1 || !slices.Equal(again.Packet, view.Packet) {
		t.Fatalf("round duplicated across restart: before %+v, after %+v", view, again)
	}
	_, err = f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: again.Revision, Note: "Owner ruling for the requester."})
	fixturekit.Must(t, err)
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	ruling := f.Ruling(t, stream, intake.MasonAgent("resume"))
	if len(ruling) != 1 || !strings.Contains(ruling[0].Request.Prompt, "Owner ruling for the requester.") {
		t.Fatalf("requester ruling %+v", ruling)
	}
	after, _, _, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if after.Seal != before.Seal+1 || !slices.Equal(f.Revisions(t, stream, plan.SpecDocument), []int{1, 2}) {
		t.Fatalf("approved amendment left seal %+v, spec revisions %v", after, f.Revisions(t, stream, plan.SpecDocument))
	}
	f.AwaitTransition(t, stream, intake.AmendmentUnitID("upload", "1", true), intake.UnitReviewing, intake.UnitImplementing)
	if state, err := f.UnitState(stream, "audit"); err != nil || state != intake.UnitReviewing {
		t.Fatalf("unaffected audit unit: %s, %v", state, err)
	}
	if moves := intakefixture.UnitMoves(t, f.Repository(), stream, "audit"); len(moves) != 0 {
		t.Fatalf("unaffected audit unit was reconsidered: %+v", moves)
	}
	if moved := f.Transition(t, stream, trace.UnitSubject("resume")+"_resumed_amendment_1"); moved.To != intake.UnitImplementing {
		t.Fatalf("requester did not resume: %+v", moved)
	}
	masons.Check(t)
	f.Stop(t)
	f.Start(t)
	if turns := f.Ruling(t, stream, intake.MasonAgent("resume")); len(turns) != 1 {
		t.Fatalf("requester ruling after restart %+v", turns)
	}
}
