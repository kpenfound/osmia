package lifecycle

import (
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	ArchivePoll = "archive"
	EdgePoll    = "edge"
)

type pullRequestPollKey struct {
	Project config.ProjectID
	Stream  config.WorkstreamID
	Kind    string
}
type PullRequestAttempt struct {
	At      time.Time
	Request string
}

// PullRequestAttempts is cadence state only. Reload retains it; restart may
// reset it. Resolution and edge release read durable evidence instead.
type PullRequestAttempts struct {
	mu       sync.Mutex
	attempts map[pullRequestPollKey]PullRequestAttempt
}

func (p *PullRequestAttempts) Last(project config.ProjectID, stream config.WorkstreamID, kind string) (PullRequestAttempt, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.attempts[pullRequestPollKey{project, stream, kind}]
	return a, ok
}
func (p *PullRequestAttempts) Record(project config.ProjectID, stream config.WorkstreamID, kind string, attempt PullRequestAttempt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts == nil {
		p.attempts = make(map[pullRequestPollKey]PullRequestAttempt)
	}
	p.attempts[pullRequestPollKey{project, stream, kind}] = attempt
}
func (p *PullRequestAttempts) Forget(project config.ProjectID, stream config.WorkstreamID, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.attempts, pullRequestPollKey{project, stream, kind})
}

// EdgePollRequest uses trace order so a request and observation with equal
// clock timestamps still consume the immediate check exactly once.
func EdgePollRequest(repo *trace.Repository, stream config.WorkstreamID) (request string, pending bool, err error) {
	records, err := trace.Read[trace.Record](repo, stream)
	if err != nil {
		return "", false, err
	}
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			if r.Subject == DriftRequestSubject && r.Actor == ownerActor {
				request, pending = r.ID, true
			}
		case trace.Document:
			if r.ID == EdgeObservationDocument {
				pending = false
			}
		}
	}
	return request, pending, nil
}
