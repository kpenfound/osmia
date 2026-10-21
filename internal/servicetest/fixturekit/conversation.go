package fixturekit

import (
	"context"
	"sync"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// ChiefTurns is a fake chief of staff. It answers each prompt, and the first
// turn it runs waits for hold to close when hold is set.
type ChiefTurns struct {
	Mu       sync.Mutex
	Prepared []coreadapter.PreparedTurn
	Hold     chan struct{}
	Entered  chan struct{}
}

func (c *ChiefTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	c.Mu.Lock()
	c.Prepared = append(c.Prepared, p)
	first := len(c.Prepared) == 1
	c.Mu.Unlock()
	if first && c.Hold != nil {
		close(c.Entered)
		select {
		case <-c.Hold:
		case <-ctx.Done():
			return coreadapter.SessionResult{}, ctx.Err()
		}
	}
	session := coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "session"}
	switch p.Prompt {
	case "fail":
		return coreadapter.SessionResult{Session: session, FinalResponse: "partial", IsError: true}, nil
	case "cancel":
		return coreadapter.SessionResult{Session: session, FinalResponse: "stopped", Cancelled: true}, nil
	case "silent":
		return coreadapter.SessionResult{Session: session}, nil
	}
	return coreadapter.SessionResult{Session: session, FinalResponse: "Answer to " + p.Prompt}, nil
}

func (c *ChiefTurns) CheckResume(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
	return nil
}

func (c *ChiefTurns) Calls() []coreadapter.PreparedTurn {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	return append([]coreadapter.PreparedTurn(nil), c.Prepared...)
}
