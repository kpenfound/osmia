package questions

import (
	"context"
	"errors"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// Waiting is the outcome of a turn that asked a question or filed an amendment.
const Waiting = "waiting"

// Turns ends every turn that asked a question or filed an amendment with the outcome waiting,
// whatever the agent reported, so the thread parks and its slot is released.
// The recorded question decides, not the agent's session.
type Turns struct {
	Turns      coreadapter.Turns
	Repository *trace.Repository
}

var _ coreadapter.Turns = (*Turns)(nil)
var _ coreadapter.ResumeChecker = (*Turns)(nil)

func (t *Turns) Run(ctx context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	if t.Turns == nil || t.Repository == nil {
		return coreadapter.SessionResult{}, errors.New("question turns require a runner and a trace")
	}
	result, err := t.Turns.Run(ctx, prepared)
	states, readErr := t.Repository.Questions(config.WorkstreamID(prepared.Scope.Workstream))
	if readErr != nil {
		return result, errors.Join(err, readErr)
	}
	for _, q := range states {
		if q.Asked.Thread == prepared.Scope.Thread && q.Asked.Turn == prepared.Scope.Turn {
			result.Outcome = &coreadapter.Outcome{Status: Waiting, Report: "Asked question " + q.Asked.ID}
		}
	}
	amendments, readErr := trace.Read[trace.Amendment](t.Repository, config.WorkstreamID(prepared.Scope.Workstream))
	if readErr != nil {
		return result, errors.Join(err, readErr)
	}
	for _, a := range amendments {
		if a.Thread == prepared.Scope.Thread && a.Turn == prepared.Scope.Turn && a.Role != trace.ChiefOfStaff {
			result.Outcome = &coreadapter.Outcome{Status: Waiting, Report: "Filed amendment " + a.ID}
		}
	}
	return result, err
}

// CheckResume defers to the wrapped runner; one that cannot check resumes
// makes the turn replay.
func (t *Turns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := t.Turns.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	return coreadapter.ErrResumeUnavailable
}
