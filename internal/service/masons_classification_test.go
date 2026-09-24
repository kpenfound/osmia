package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
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
			f.engine.mu.Lock()
			for _, name := range f.engine.runs {
				if strings.Contains(name, "-classifier-") {
					t.Errorf("classifier ran without a configured profile: %s", name)
				}
			}
			f.engine.mu.Unlock()
		})
	}
}

func TestMasonModelClassifier(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replies  []string
		failed   bool
		want     string
		attempts int
	}{
		{"valid", []string{`{"class":"claims_done","evidence":"finished"}`}, false, "claims_done", 1},
		{"invalid", []string{`invalid`, `{"class":"gave_up","evidence":"cannot continue"}`}, false, "gave_up", 2},
		{"exhausted", []string{`invalid`, `invalid`}, false, "unclear", 2},
		{"failed", nil, true, "unclear", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fake := newConfiguredMasonFixture(t, "masons = 1\n", validPlan, "default")
			defer f.stop(t)
			fake.response = map[string]string{masonTurnID("resume"): "Edited the file."}
			f.engine.mu.Lock()
			for i := 1; i <= tc.attempts; i++ {
				i := i
				f.engine.turns[masonTurnID("resume")+"-classifier-"+string(rune('0'+i))] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
					if req.Profile.Name != "classifier" {
						t.Errorf("classifier profile: %+v", req.Profile)
					}
					if tc.failed {
						return &agent.Result{ClaudeID: "classifier", SessionDir: req.SessionDir, ResultText: "unavailable", NumTurns: 1, CostUSD: .02, CostKnown: true, IsError: true}, nil
					}
					return &agent.Result{ClaudeID: "classifier", SessionDir: req.SessionDir, ResultText: tc.replies[i-1], NumTurns: 1, CostUSD: .02, CostKnown: true}, nil
				}
			}
			f.engine.mu.Unlock()
			stream, _ := f.builtAs(t, "model-classifier")
			th := f.awaitMasonRan(t, stream, "resume")
			if got := th.Turns[0].Response.Classification.Class; got != tc.want {
				f.engine.mu.Lock()
				runs := append([]string(nil), f.engine.runs...)
				f.engine.mu.Unlock()
				t.Fatalf("class %s want %s; runs %v; response %+v", got, tc.want, runs, th.Turns[0].Response)
			}
			if got := len(th.Turns[0].Response.ClassifierUsage); got != tc.attempts {
				t.Fatalf("usage count %d", got)
			}
			costs, err := trace.Read[trace.Cost](f.repository(), stream)
			must(t, err)
			count := 0
			for _, cost := range costs {
				if cost.Entry.Scope.Turn == masonTurnID("resume") && cost.Entry.AttemptID != "" && cost.Entry.Usage == (coreadapter.Usage{CostUSD: .02, CostKnown: true, Turns: 1}) {
					count++
				}
			}
			if !tc.failed && count != tc.attempts {
				t.Fatalf("classifier costs %d want %d", count, tc.attempts)
			}
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
