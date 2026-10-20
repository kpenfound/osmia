package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// escalationGate holds the first escalation before its durable effect until
// shutdown cancels the session. Later calls use the ordinary scoped handler.
type escalationGate struct {
	base    *demoTransport
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
	p := &faults{}
	gate := &escalationGate{entered: make(chan struct{})}
	f, chief := newAskingFixtureWith(t, 1, 1, p, func(opts *Options) {
		gate.base = opts.Committee.Hosts.(*coreadapter.MCPHost).Transport.(*demoTransport)
		opts.Threads = Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: &coreadapter.MCPHost{Transport: gate}}).Threads
	})
	t.Cleanup(func() { f.stop(t) })
	cancelled := make(chan error, 1)
	var deliveries atomic.Int32
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		first := strings.Contains(req.Prompt, "Question 1 is open") && deliveries.Add(1) == 1
		result, err := chief.turn(ctx, req, turn, tools)
		if first {
			cancelled <- err
		}
		return result, err
	}
	f.engine.mu.Unlock()
	asking := f.member(1, 1, 1, asks(p, "1"))
	stream := f.handIn(t, "cancelled-chief", handedDesign)
	awaitStart(t, gate.entered)
	f.awaitCompleted(t, stream, committeeAgent(1), asking)
	if q := f.question(t, stream, "1"); q.State != trace.QuestionOpen {
		t.Fatalf("question before cancelled escalation: %+v", q)
	}
	f.stop(t)
	// A transport teardown error must not mask a fake session that falsely
	// returns success after its tool call was cancelled.
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled chief session returned %v, want context.Canceled", err)
	}
	f.start(t)
	f.awaitQuestion(t, stream, "1", trace.QuestionEscalated)
	conversationAwait(t, func() bool {
		outbox, err := f.repository().Outbox(stream)
		must(t, err)
		for _, entry := range outbox {
			if strings.Contains(entry.Event.Body, "Question 1 is open") {
				return entry.Acknowledged
			}
		}
		return false
	})
	inbox, err := f.c.Inbox(context.Background())
	must(t, err)
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
	for _, move := range f.transitions(t, stream) {
		if move.Subject == trace.QuestionSubject("1") && move.To == trace.QuestionEscalated {
			escalations++
		}
	}
	if escalations != 1 {
		t.Fatalf("durable escalations: %d", escalations)
	}
	p.check(t)
}
