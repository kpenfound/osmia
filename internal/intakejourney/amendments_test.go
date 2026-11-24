package intakejourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	questions "github.com/kpenfound/osmia/internal/questions"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRoutedAmendmentFreesSlotForAnotherUnit(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewParallelMasonFixture(t, 1, 1, journey.ParallelPlan)
	defer f.Stop(t)
	masons.Chief.Route("1", []string{"spec#1", "plan#resume"}, "Clarify restart state", "The task needs another state")
	masons.Play[execution.MasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		listed, err := tools.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		for _, tool := range listed.Tools {
			if tool.Name == questions.AmendTool {
				t.Errorf("a unit mason holds %s", questions.AmendTool)
			}
		}
		got, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "Restart state is not in the spec. Should it be?"})
		if err != nil || !strings.Contains(got, `"question":"1"`) {
			t.Errorf("ask: %q %v", got, err)
		}
		return nil
	}
	stream, _ := f.BuiltAs(t, "amend-slot")
	f.S.Until(t, "unit resume to wait", func() bool {
		state, err := f.UnitState(stream, "resume")
		if err != nil {
			t.Fatal(err)
		}
		return state == intake.UnitWaiting
	})
	f.AwaitMasonRan(t, stream, "upload")
	if state, err := f.UnitState(stream, "dedupe"); err != nil || state != intake.UnitReady {
		t.Fatalf("entangled unit state %q: %v", state, err)
	}
	var requests []trace.Amendment
	f.S.Until(t, "the chief of staff to route the question", func() bool {
		var err error
		requests, err = trace.Read[trace.Amendment](f.Repository(), stream)
		if err != nil {
			t.Fatal(err)
		}
		return len(requests) > 0
	})
	if len(requests) != 1 {
		t.Fatalf("requests %+v", requests)
	}
	if requests[0].Unit != "resume" || requests[0].Role != trace.ChiefOfStaff || requests[0].Requester.ID != intake.MasonAgent("resume") || requests[0].QuestionID != "1" {
		t.Fatalf("request %+v", requests[0])
	}
	masons.Check(t)
}
