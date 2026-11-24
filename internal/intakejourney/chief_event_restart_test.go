package intakejourney

import (
	"context"
	"errors"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type escalationGate struct {
	base    *fixturekit.DemoTransport
	entered chan struct{}
	calls   atomic.Int32
}

func (g *escalationGate) Start(ctx context.Context, server *mcp.Server) (coreadapter.Endpoint, coreadapter.Lease, error) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(callCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && method == "tools/call" && params.Name == questions.EscalateTool {
				if g.calls.Add(1) == 1 {
					close(g.entered)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			return next(callCtx, method, req)
		}
	})
	return g.base.Start(ctx, server)
}

func TestCancelledChiefEventRedeliversQuestionAfterRestart(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	gate := &escalationGate{entered: make(chan struct{})}
	f, chief := newAskingFixtureWith(t, 1, 1, p, func(opts *journey.Options) {
		gate.base = opts.Committee.Hosts.(*coreadapter.MCPHost).Transport.(*fixturekit.DemoTransport)
		opts.Threads = journey.Enforce(*opts, service.Enforcement{Engine: opts.Committee.Engine, Hosts: &coreadapter.MCPHost{Transport: gate}}).Threads
	})
	t.Cleanup(func() { f.Stop(t) })
	cancelled := make(chan error, 1)
	var deliveries atomic.Int32
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		first := strings.Contains(req.Prompt, "Question 1 is open") && deliveries.Add(1) == 1
		result, err := chief.Turn(ctx, req, turn, tools)
		if first {
			cancelled <- err
		}
		return result, err
	}
	f.Engine.Mu.Unlock()
	asking := f.Member(1, 1, 1, intakefixture.Asks(p, "1"))
	stream := f.HandIn(t, "cancelled-chief", fixturekit.HandedDesign)
	intakefixture.AwaitStart(t, gate.entered)
	intakefixture.Soon(t, "the asking turn to complete", func() bool {
		return !f.AwaitTurn(t, stream, intake.CommitteeAgent(1), asking).CompletedAt.IsZero()
	})
	if q := f.Question(t, stream, "1"); q.State != trace.QuestionOpen {
		t.Fatalf("question before cancelled escalation: %+v", q)
	}
	f.Stop(t)
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled chief session returned %v, want context.Canceled", err)
	}
	f.Start(t)
	f.AwaitQuestion(t, stream, "1", trace.QuestionEscalated)
	intakefixture.Soon(t, "the question event to be acknowledged", func() bool {
		outbox, err := f.Repository().Outbox(stream)
		fixturekit.Must(t, err)
		for _, entry := range outbox {
			if strings.Contains(entry.Event.Body, "Question 1 is open") {
				return entry.Acknowledged
			}
		}
		return false
	})
	inbox, err := f.C.Inbox(context.Background())
	fixturekit.Must(t, err)
	entries := 0
	for _, entry := range inbox.Entries {
		for _, q := range entry.Asked {
			if q.ID == "1" {
				entries++
			}
		}
	}
	if entries != 1 || gate.calls.Load() != 2 || deliveries.Load() != 2 {
		t.Fatalf("question deliveries %d, escalation calls %d, inbox entries %d: %+v", deliveries.Load(), gate.calls.Load(), entries, inbox.Entries)
	}
	escalations := 0
	for _, move := range f.Transitions(t, stream) {
		if move.Subject == trace.QuestionSubject("1") && move.To == trace.QuestionEscalated {
			escalations++
		}
	}
	if escalations != 1 {
		t.Fatalf("durable escalations: %d", escalations)
	}
	p.Check(t)
}
