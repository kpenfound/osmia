package clientjourney

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const keptByTheLostSession = "package trace\n\n// written before the client was lost\n"

func TestAClientWhoseLeaseEndsHasItsTurnRecoveredFromTheViewItReturns(t *testing.T) {
	r := newServerRig(t, single("masons = 1\nlocal_slots = 0\n", fixturekit.ValidPlan))
	m := r.machine("node-lost")
	f := r.f
	recoverTurn := intake.MasonAgent("resume") + "-recover-1"
	var mu sync.Mutex
	var recoveredEdit string
	entered, cancelled := make(chan struct{}), make(chan struct{})
	m.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		req := turn.Request
		dir := req.Workspace.Directory()
		if !strings.HasPrefix(req.Name, intake.MasonAgent("resume")) {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Noted", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if req.Name == recoverTurn {
			data, _ := os.ReadFile(filepath.Join(dir, intakefixture.TrackedFile))
			mu.Lock()
			recoveredEdit = string(data)
			mu.Unlock()
			tools, err := dial(ctx, req)
			if err != nil {
				return nil, err
			}
			defer tools.Close()
			if err := intakefixture.ReportDone("Continued from the returned view")(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Continued", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if err := write(dir, intakefixture.TrackedFile, keptByTheLostSession); err != nil {
			return nil, err
		}
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return &agent.Result{ClaudeID: "session-lost", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
	}
	m.start(r.address(), 2)
	r.awaitCapable("node-lost")

	stream := build(f, t, "lost", fixturekit.ValidPlan)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatalf("the turn never ran on the client\n%s", m.logs.String())
	}

	m.net.down.Store(true)
	r.leases.Advance(time.Minute)
	r.f.S.Fleet().Expire()
	if _, ok := r.connected("node-lost"); ok {
		t.Fatal("the lease did not end")
	}
	select {
	case <-cancelled:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the client did not cancel its session past the grace window")
	}
	m.net.down.Store(false)

	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)
	report := latestReport(t, f.Repository(), stream, "resume")
	if got, err := inCommit(f, report.Candidate, intakefixture.TrackedFile); err != nil || got != keptByTheLostSession {
		t.Fatalf("the unit's candidate %q does not hold the view the lost client returned: %q %v", report.Candidate, got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if recoveredEdit != keptByTheLostSession {
		t.Fatalf("the recovery turn did not start from the returned view: %q", recoveredEdit)
	}
	th, err := f.Repository().Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	first := th.Turns[0]
	if first.Request.TurnID != execution.MasonTurnID("resume") || first.Response == nil || first.Response.Result.Client != "node-lost" {
		t.Fatalf("the interrupted turn is not recorded against its client: %+v", first.Response)
	}
	intakefixture.Soon(t, "the lost client's late result to be recorded as it arrived", func() bool {
		events, err := f.Repository().ClientEvents(stream, execution.MasonTurnID("resume"))
		fixturekit.Must(t, err)
		for _, e := range events {
			if e.Kind == trace.ClientLate && e.What == "result" && e.Client == "node-lost" {
				return true
			}
		}
		return false
	})
	if th.Turns[0].Response.Result.Outcome != nil {
		t.Fatalf("a late result changed the recorded turn: %+v", th.Turns[0].Response.Result)
	}
}
