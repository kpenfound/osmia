package adaptertest

import (
	"context"
	"sync"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest/enforcertest"
	a "github.com/kpenfound/osmia/internal/coreadapter"
)

// Engine hands out core's fake enforcer, which checks grants and requests
// with core's own code and launches no process.
type Engine struct {
	// Enforcers holds every enforcer handed out, Prepared every session
	// prepared, and Requests every admitted request that ran.
	Enforcers []*enforcertest.Enforcer
	Prepared  []agent.Session
	Requests  []agent.Request
	// EnforcerErr is returned by Enforcer, PrepareErr by Prepare and RunErr by
	// the fake agent.
	EnforcerErr, PrepareErr, RunErr error
	// Policy alters the policy of every prepared session.
	Policy func(*agent.Policy)
	OnRun  func() error
	// Resume answers CheckResume when set; otherwise the saved session is unavailable.
	Resume       func(previous, next a.Profile, session a.BackendSession) error
	ResumeChecks int

	mu sync.Mutex
}

func (f *Engine) CheckResume(ctx context.Context, previous, next a.Profile, session a.BackendSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.ResumeChecks++
	if f.Resume == nil {
		return a.ErrResumeUnavailable
	}
	return f.Resume(previous, next, session)
}

// Released reports whether every prepared session was released.
func (f *Engine) Released() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.Enforcers {
		for _, s := range e.Sessions() {
			if !s.Released() {
				return false
			}
		}
	}
	return true
}

func (f *Engine) Enforcer(settings a.ExecutionSettings) (agent.Enforcer, error) {
	if f.EnforcerErr != nil {
		return nil, f.EnforcerErr
	}
	fake := &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image, PrepareErr: f.PrepareErr,
		Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.Requests = append(f.Requests, turn.Request)
			f.mu.Unlock()
			if f.OnRun != nil {
				if err := f.OnRun(); err != nil {
					return nil, err
				}
			}
			if f.RunErr != nil {
				return nil, f.RunErr
			}
			return &agent.Result{ClaudeID: "fixture-session", ResultText: "fixture response", SessionDir: turn.Request.SessionDir}, nil
		}}
	f.mu.Lock()
	f.Enforcers = append(f.Enforcers, fake)
	f.mu.Unlock()
	return enforcer{f, fake}, nil
}

type enforcer struct {
	f    *Engine
	fake *enforcertest.Enforcer
}

func (e enforcer) Prepare(ctx context.Context, grants agent.Grants) (agent.Session, error) {
	s, err := e.fake.Prepare(ctx, grants)
	if err != nil {
		return nil, err
	}
	e.f.mu.Lock()
	e.f.Prepared = append(e.f.Prepared, s)
	e.f.mu.Unlock()
	if e.f.Policy == nil {
		return s, nil
	}
	return session{s, e.f.Policy}, nil
}

type session struct {
	agent.Session
	alter func(*agent.Policy)
}

func (s session) Policy() agent.Policy {
	p := s.Session.Policy()
	s.alter(&p)
	return p
}
