package fixturekit

import (
	"context"
	"sync"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// SessionEngine is a fake execution engine for a session's turns. Unlike
// DemoEngine it needs no MCP tool session, because a session turn is never
// given a custom or MCP tool; it answers CheckResume from a test-supplied
// function, defaulting to unavailable so a test that never sets it gets a
// fresh model session every turn.
type SessionEngine struct {
	Mu     sync.Mutex
	Runs   []agent.Request
	Turn   func(ctx context.Context, req agent.Request) (*agent.Result, error)
	Resume func(previous, next coreadapter.Profile, session coreadapter.BackendSession) error
}

func (e *SessionEngine) Enforcer(s coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: s.Mode, Image: s.Image, Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		e.Mu.Lock()
		e.Runs = append(e.Runs, turn.Request)
		run := e.Turn
		e.Mu.Unlock()
		if run == nil {
			return &agent.Result{ClaudeID: "session", ResultText: "ok"}, nil
		}
		return run(ctx, turn.Request)
	}}, nil
}

func (e *SessionEngine) CheckResume(_ context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	e.Mu.Lock()
	resume := e.Resume
	e.Mu.Unlock()
	if resume == nil {
		return coreadapter.ErrResumeUnavailable
	}
	return resume(previous, next, session)
}

func (e *SessionEngine) Calls() []agent.Request {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	return append([]agent.Request(nil), e.Runs...)
}
