package lifecycle

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// TestPollAttempt is a snapshot of a polling attempt for a service fixture.
type TestPollAttempt struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Kind       string
	At         time.Time
}

// AttemptsForTest reads attempt timing under the controller's lock. Scheduling
// deadlines derived from these snapshots belong to the test fixtures.
func (p *PullRequestAttempts) AttemptsForTest() []TestPollAttempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []TestPollAttempt
	for key, a := range p.attempts {
		out = append(out, TestPollAttempt{Project: key.Project, Workstream: key.Stream, Kind: key.Kind, At: a.At})
	}
	return out
}
