package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A unit mason holds no amend tool: its question, routed to an amendment by
// the chief of staff, parks the unit and frees its slot for another unit.
func TestRoutedAmendmentFreesSlotForAnotherUnit(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 1, 1, parallelPlan)
	defer f.stop(t)
	masons.chief.route("1", []string{"spec#1", "plan#resume"}, "Clarify restart state", "The task needs another state")
	masons.play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		listed, err := tools.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		for _, tool := range listed.Tools {
			if tool.Name == questions.AmendTool {
				t.Errorf("a unit mason holds %s", questions.AmendTool)
			}
		}
		got, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "Restart state is not in the spec. Should it be?"})
		if err != nil || !strings.Contains(got, `"question":"1"`) {
			t.Errorf("ask: %q %v", got, err)
		}
		return nil
	}
	stream, _ := f.builtAs(t, "amend-slot")
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.unitState(stream, "resume")
		if err != nil {
			t.Fatal(err)
		}
		if state == UnitWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit stayed %s", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.awaitMasonRan(t, stream, "upload")
	if state, err := f.unitState(stream, "dedupe"); err != nil || state != UnitReady {
		t.Fatalf("entangled unit state %q: %v", state, err)
	}
	// The chief of staff routes the question on its next turn.
	var requests []trace.Amendment
	for {
		var err error
		requests, err = trace.Read[trace.Amendment](f.repository(), stream)
		if err != nil {
			t.Fatal(err)
		}
		if len(requests) > 0 {
			break
		}
		if time.Now().After(deadline) {
			masons.chief.p.check(t)
			t.Fatal("the chief of staff never routed the question")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(requests) != 1 {
		t.Fatalf("requests %+v", requests)
	}
	if requests[0].Unit != "resume" || requests[0].Role != trace.ChiefOfStaff || requests[0].Requester.ID != masonAgent("resume") || requests[0].QuestionID != "1" {
		t.Fatalf("request %+v", requests[0])
	}
	masons.check(t)
}
