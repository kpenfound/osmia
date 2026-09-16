package adaptertest

import (
	"context"

	"github.com/kpenfound/busybees/core/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
)

// IsolationEngine records construction without launching any process. Its
// inspection report is a fixture, not evidence of host/container enforcement.
type IsolationEngine struct {
	Policies                                   []a.BoundaryPolicy
	Requests                                   []agent.Request
	PrepareErr, InspectErr, RunErr, ReleaseErr error
	Mutate                                     func(*a.BoundaryPolicy)
	OnRun                                      func() error
	Released                                   int
	// Resume answers CheckResume when set; otherwise the saved session is unavailable.
	Resume       func(previous, next a.Profile, session a.BackendSession) error
	ResumeChecks int
}

func (f *IsolationEngine) CheckResume(ctx context.Context, previous, next a.Profile, session a.BackendSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.ResumeChecks++
	if f.Resume == nil {
		return a.ErrResumeUnavailable
	}
	return f.Resume(previous, next, session)
}

func (f *IsolationEngine) Prepare(ctx context.Context, p a.BoundaryPolicy) (a.IsolatedSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Policies = append(f.Policies, p)
	if f.PrepareErr != nil {
		return nil, f.PrepareErr
	}
	return &isolatedSession{engine: f, policy: p}, nil
}

type isolatedSession struct {
	engine *IsolationEngine
	policy a.BoundaryPolicy
}

func (s *isolatedSession) Inspect(context.Context) (a.BoundaryPolicy, error) {
	if s.engine.Mutate != nil {
		s.engine.Mutate(&s.policy)
	}
	return s.policy, s.engine.InspectErr
}
func (s *isolatedSession) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	f := s.engine
	f.Requests = append(f.Requests, req)
	if f.OnRun != nil {
		if err := f.OnRun(); err != nil {
			return nil, err
		}
	}
	return &agent.Result{ClaudeID: "fixture-session", ResultText: "fixture response", SessionDir: req.SessionDir}, f.RunErr
}
func (s *isolatedSession) Release(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.engine.Released++
	return s.engine.ReleaseErr
}
