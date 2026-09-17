package adaptertest

import (
	"context"

	"github.com/kpenfound/busybees/core/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
)

// Engine verifies requests with core's own boundaries and records them
// without launching any process.
type Engine struct {
	// Verified holds every request passed to Verify; Requests those that ran.
	Verified, Requests []agent.Request
	VerifyErr, RunErr  error
	// Mutate alters the turn core verified before Verify returns it.
	Mutate func(*agent.Turn)
	OnRun  func() error
	// Resume answers CheckResume when set; otherwise the saved session is unavailable.
	Resume       func(previous, next a.Profile, session a.BackendSession) error
	ResumeChecks int
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

func (f *Engine) Verify(req agent.Request) (*agent.Turn, error) {
	f.Verified = append(f.Verified, req)
	if f.VerifyErr != nil {
		return nil, f.VerifyErr
	}
	turn, err := (&agent.Runner{}).Verify(req)
	if err != nil {
		return nil, err
	}
	if f.Mutate != nil {
		f.Mutate(turn)
	}
	return turn, nil
}

// Run verifies the request again, as core's runner does, before recording it.
func (f *Engine) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := (&agent.Runner{}).Verify(req); err != nil {
		return nil, err
	}
	f.Requests = append(f.Requests, req)
	if f.OnRun != nil {
		if err := f.OnRun(); err != nil {
			return nil, err
		}
	}
	return &agent.Result{ClaudeID: "fixture-session", ResultText: "fixture response", SessionDir: req.SessionDir}, f.RunErr
}
