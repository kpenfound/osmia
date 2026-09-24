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

func TestMasonAmendmentFreesSlotForAnotherUnit(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		got, err := callTool(ctx, tools, questions.AmendTool, map[string]any{"citations": []string{"spec#1", "plan#resume"}, "change": "Clarify restart state", "reason": "The planned proof needs another state"})
		if err != nil || !strings.Contains(got, `"amendment":"1"`) {
			t.Errorf("amend: %q %v", got, err)
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
	f.awaitMasonRan(t, stream, "dedupe")
	requests, err := trace.Read[trace.Amendment](f.repository(), stream)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests %+v: %v", requests, err)
	}
	if requests[0].Unit != "resume" || requests[0].Role != "mason" {
		t.Fatalf("request %+v", requests[0])
	}
	masons.check(t)
}
