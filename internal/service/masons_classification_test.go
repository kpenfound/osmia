package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMasonCleanTurnPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, response, class, tool string
		contested                   bool
	}{
		{"question", "Could you clarify which store to use?", "asked_in_prose", "ask", false},
		{"completion", "I have completed the unit.", "claims_done", "done", false},
		{"giveup", "I cannot complete this work.", "gave_up", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, fake := newMasonFixture(t, 1, validPlan)
			defer f.stop(t)
			fake.response = map[string]string{masonTurnID("resume"): tc.response}
			fake.play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
				_, err := callTool(ctx, tools, "file_read", map[string]any{"path": masonWrote})
				return err
			}
			delete(fake.play, masonAgent("resume")+"-clarify-1")
			f.engine.mu.Lock()
			f.engine.turns[masonAgent("resume")+"-clarify-1"] = fake.turn
			f.engine.mu.Unlock()
			stream, _ := f.builtAs(t, "classified")
			deadline := time.Now().Add(demoTimeout)
			for {
				th, err := f.repository().Thread(stream, masonAgent("resume"))
				if err == nil && len(th.Turns) > 0 && th.Turns[0].Response != nil && th.Turns[0].Response.Classification != nil {
					if got := th.Turns[0].Response.Classification.Class; got != tc.class {
						t.Fatalf("class %s, want %s", got, tc.class)
					}
					if got := th.Turns[0].Response.Classification.ToolCounts["file_read"]; got != 1 {
						t.Fatalf("tool count %d, want 1", got)
					}
					if tc.contested {
						state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
						if err == nil && state.Value == UnitContested {
							break
						}
					} else if len(th.Turns) > 1 && strings.Contains(th.Turns[1].Request.Prompt, "Call "+tc.tool) && th.Turns[1].Request.ThreadID == th.Identity.ThreadID {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("policy did not settle: %+v %v", th, err)
				}
				time.Sleep(50 * time.Millisecond)
			}
			f.awaitEventTurns(t, stream, "classified "+tc.class)
		})
	}
}

func TestMasonCleanTurnBound(t *testing.T) {
	t.Parallel()
	f, fake := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	f.engine.mu.Lock()
	for i := 1; i < f.s.cfg.Mason.MaxCleanTurns; i++ {
		name := masonAgent("resume") + "-clarify-" + string(rune('0'+i))
		delete(fake.play, name)
		f.engine.turns[name] = fake.turn
	}
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "bounded")
	f.awaitUnit(t, stream, "resume", UnitContested)
	th, err := f.repository().Thread(stream, masonAgent("resume"))
	if err != nil || len(th.Turns) != f.s.cfg.Mason.MaxCleanTurns {
		t.Fatalf("bounded turns: %d %v", len(th.Turns), err)
	}
	transition := f.transition(t, stream, trace.EventID(th.Turns[len(th.Turns)-1].Response.ID, "contested"))
	if !strings.Contains(transition.Reason, "bound exhausted") {
		t.Fatalf("contest reason: %s", transition.Reason)
	}
	f.awaitEventTurns(t, stream, "classified unclear")
	f.awaitEventTurns(t, stream, "bound exhausted")
}
