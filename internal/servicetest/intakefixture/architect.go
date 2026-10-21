package intakefixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ArchitectScenario scripts architect and committee turns against a live repository.
// The callbacks follow the adapter's current service across restarts.
type ArchitectScenario struct {
	Client     func() WorkflowClient
	Engine     *fixturekit.DemoEngine
	Repository func() *trace.Repository
	Changes    func(*testing.T) (func(), func())
	statusMu   sync.Mutex
	statuses   map[string][]intake.DraftStatus
}

// Script installs the fake architect's behaviour for one turn: it delivers
// the given files and returns a successful result.
func (f *ArchitectScenario) Script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		var err error
		if check != nil {
			err = check(ctx, req, verified, tools)
		}
		for _, path := range []string{plan.SpecPath, plan.PlanPath, "decline.txt"} {
			content, ok := files[path]
			if !ok {
				continue
			}
			got, e := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": path, "content": content})
			var status intake.DraftStatus
			if e == nil {
				e = json.Unmarshal([]byte(got), &status)
			}
			if e != nil {
				err = errors.Join(err, fmt.Errorf("deliver %s: %w", path, e))
				continue
			}
			f.statusMu.Lock()
			if f.statuses == nil {
				f.statuses = map[string][]intake.DraftStatus{}
			}
			f.statuses[turn] = append(f.statuses[turn], status)
			f.statusMu.Unlock()
		}
		if err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Draft delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// Delivered returns what each draft_write the scripted turn made returned.
func (f *ArchitectScenario) Delivered(turn string) []intake.DraftStatus {
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	return slices.Clone(f.statuses[turn])
}

func (f *ArchitectScenario) Runs() []string {
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	return slices.Clone(f.Engine.Runs)
}

// Await polls the workstream's feature and draft states until want accepts
// them.
func (f *ArchitectScenario) Await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	wait, close := f.Changes(t)
	defer close()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		feature, err := f.Repository().Workflow(stream, trace.FeatureSubject)
		fixturekit.Must(t, err)
		draft, err := f.Repository().Workflow(stream, intake.DraftSubject)
		fixturekit.Must(t, err)
		if want(feature.Value, draft.Value) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s stayed %q with draft %q", stream, feature.Value, draft.Value)
		}
		wait()
	}
}

func (f *ArchitectScenario) TransitionRecorded(t *testing.T, stream config.WorkstreamID, id string) func(string, string) bool {
	return func(_, _ string) bool {
		return slices.ContainsFunc(f.Transitions(t, stream), func(tr trace.Transition) bool { return tr.ID == id })
	}
}

func (f *ArchitectScenario) Documents(t *testing.T, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.Repository(), stream)
	fixturekit.Must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}

func (f *ArchitectScenario) Transitions(t *testing.T, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	Transitions, err := trace.Read[trace.Transition](f.Repository(), stream)
	fixturekit.Must(t, err)
	return Transitions
}

func (f *ArchitectScenario) DraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.Repository().Operations(stream)
	fixturekit.Must(t, err)
	var out []trace.OperationRecord
	for _, o := range ops {
		if o.Operation.Action == intake.DraftAction {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	return out
}

// AcknowledgedDraftOperations returns the draft operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *ArchitectScenario) AcknowledgedDraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.DraftOperations(t, stream) })
}

func (f *ArchitectScenario) ArchitectThread(t *testing.T, stream config.WorkstreamID) trace.Thread {
	t.Helper()
	th, err := f.Repository().Thread(stream, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	return th
}

// Member installs the fake behaviour of one Member's turn. Errors it returns
// fail the turn, so checks collect theirs for the test to read.
func (f *ArchitectScenario) Member(round, i, attempt int, run func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) string {
	turn := intake.RoundTurnID(round, intake.CommitteeAgent(i), attempt)
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := run(ctx, req, verified, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Round read", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	return turn
}

// AwaitShed waits until the workstream's shed has reached the wanted state,
// whatever it moved to since. It fails at once when the shed reaches a state
// in never, after which the wanted one cannot come: by default, for a heard
// round, its failure, and for a conclusion, the next round.
func (f *ArchitectScenario) AwaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	wait, close := f.Changes(t)
	defer close()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	if kind, n, ok := intake.ShedState(want); ok && kind == "heard" && len(never) == 0 {
		never = []string{fmt.Sprintf("failed-%d", n)}
	} else if ok && kind == "concluded" && len(never) == 0 {
		never = []string{fmt.Sprintf("round-%d", n+1)}
	}
	for {
		var reached []string
		for _, tr := range f.Transitions(t, stream) {
			if tr.Subject == intake.ShedSubject {
				reached = append(reached, tr.To)
			}
		}
		if slices.Contains(reached, want) {
			return
		}
		if slices.ContainsFunc(never, func(state string) bool { return slices.Contains(reached, state) }) {
			t.Fatalf("workstream %s shed went %v, want %q", stream, reached, want)
		}
		if time.Now().After(deadline) {
			feature, _ := f.Repository().Workflow(stream, trace.FeatureSubject)
			t.Fatalf("workstream %s stayed %q with shed %v, want %q", stream, feature.Value, reached, want)
		}
		wait()
	}
}

func (f *ArchitectScenario) RoundOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.Repository().Operations(stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.RoundAction })
}

// AcknowledgedRoundOperations returns the round operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *ArchitectScenario) AcknowledgedRoundOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.RoundOperations(t, stream) })
}
