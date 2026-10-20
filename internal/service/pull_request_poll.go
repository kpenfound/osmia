package service

import (
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	archivePoll = "archive"
	edgePoll    = "edge"
)

type pullRequestPollKey struct {
	project config.ProjectID
	stream  config.WorkstreamID
	kind    string
}

type pullRequestAttempt struct {
	at      time.Time
	request string
}

// pullRequestAttempts is cadence state only. Reload retains it; restart may
// reset it. Resolution and edge release read durable evidence instead.
type pullRequestAttempts struct {
	mu       sync.Mutex
	attempts map[pullRequestPollKey]pullRequestAttempt
}

func (p *pullRequestAttempts) last(project config.ProjectID, stream config.WorkstreamID, kind string) (pullRequestAttempt, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.attempts[pullRequestPollKey{project, stream, kind}]
	return a, ok
}

func (p *pullRequestAttempts) record(project config.ProjectID, stream config.WorkstreamID, kind string, attempt pullRequestAttempt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts == nil {
		p.attempts = make(map[pullRequestPollKey]pullRequestAttempt)
	}
	p.attempts[pullRequestPollKey{project, stream, kind}] = attempt
}

func (p *pullRequestAttempts) forget(project config.ProjectID, stream config.WorkstreamID, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.attempts, pullRequestPollKey{project, stream, kind})
}

// edgePollRequest uses trace order so a request and observation with equal
// clock timestamps still consume the immediate check exactly once.
func edgePollRequest(repo *trace.Repository, stream config.WorkstreamID) (request string, pending bool, err error) {
	records, err := trace.Read[trace.Record](repo, stream)
	if err != nil {
		return "", false, err
	}
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			if r.Subject == driftRequestSubject && r.Actor == ownerActor {
				request, pending = r.ID, true
			}
		case trace.Document:
			if r.ID == edgeObservationDocument {
				pending = false
			}
		}
	}
	return request, pending, nil
}
