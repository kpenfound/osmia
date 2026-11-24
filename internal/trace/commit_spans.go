package trace

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// CommitDetail is what a span observer is given about one commit: the
// intervals the commit closed and the records that describe them, read from
// the trace as it was committed. Every time in it is a recorded time. Nothing
// in it may be modified.
type CommitDetail struct {
	Project config.ProjectID
	// Workstream is the workstream the commit belongs to, empty for the
	// project's own trace.
	Workstream config.WorkstreamID
	// Closed lists the feature and unit state intervals the commit's
	// transitions ended, in commit order.
	Closed []ClosedState
	// Turns lists the turns whose response the commit recorded.
	Turns []FinishedTurn
	// Documents are the documents the commit recorded.
	Documents []Document
}

// ClosedState is a state interval ended by Transition. The interval began
// when the workflow entered the state Transition leaves: at Entered's time, or,
// when the subject had no earlier transition, at Created.
type ClosedState struct {
	Transition Transition
	// Entered is the transition that began the interval, nil when none did.
	Entered *Transition
	// Created is the workstream's creation time, set only for the first
	// interval of the feature subject, which Entered does not begin.
	Created time.Time
}

// FinishedTurn is one turn and its recorded response.
type FinishedTurn struct {
	Request  TurnRequest
	Response TurnResponse
	// Role is the role of the thread's agent.
	Role string
	// Profile is the profile the turn's last attempt ran with.
	Profile coreadapter.Profile
	// ClaimedAt is when a runner claimed the turn.
	ClaimedAt time.Time
}

// ObserveSpans calls f with the detail of each commit this handle publishes,
// and of each commit a workstream instance of it publishes, replacing any
// earlier span observer. Observe is for notifications; this is its own hook so
// that neither replaces the other. f runs while the committing handle's lock is
// held, so it must return promptly and must not call the repository. A panic in
// f is contained: a span observer never fails a commit.
func (r *Repository) ObserveSpans(f func(CommitDetail)) {
	r.spanObserver.Store(&f)
}

func (r *Repository) spansObserved() bool {
	if f := r.spanObserver.Load(); f != nil && *f != nil {
		return true
	}
	return r.parent != nil && r.parent.spansObserved()
}

func (r *Repository) reportSpans(d CommitDetail) {
	if f := r.spanObserver.Load(); f != nil && *f != nil {
		func() {
			defer func() { _ = recover() }()
			(*f)(d)
		}()
	}
	if r.parent != nil {
		r.parent.reportSpans(d)
	}
}

func (r *Repository) commitDetail(c change, stream config.WorkstreamID, prior *cachedWorkflow) (d CommitDetail) {
	d = CommitDetail{Project: r.project, Workstream: stream}
	defer func() {
		if recover() != nil {
			d = CommitDetail{Project: r.project, Workstream: stream}
		}
	}()
	var earlier []Transaction
	if prior != nil {
		earlier = prior.log.Transactions
	}
	var batch []Transition
	for _, rec := range c.records {
		switch v := rec.(type) {
		case Transition:
			if closed, ok := r.closedState(v, earlier, batch, stream); ok {
				d.Closed = append(d.Closed, closed)
			}
			batch = append(batch, v)
		case TurnResponse:
			if c.workflow == nil {
				continue
			}
			if turn, ok := finishedTurn(c.workflow.Threads[v.AgentID], v); ok {
				d.Turns = append(d.Turns, turn)
			}
		case Document:
			d.Documents = append(d.Documents, v)
		}
	}
	return d
}

func (r *Repository) closedState(t Transition, earlier []Transaction, batch []Transition, stream config.WorkstreamID) (ClosedState, bool) {
	feature := t.Subject == FeatureSubject
	if !feature && (t.Unit == "" || t.Subject != UnitSubject(t.Unit)) {
		return ClosedState{}, false
	}
	var entered *Transition
	for i := len(batch) - 1; i >= 0 && entered == nil; i-- {
		if batch[i].Subject == t.Subject {
			entered = &batch[i]
		}
	}
	for i := len(earlier) - 1; i >= 0 && entered == nil; i-- {
		if earlier[i].Transition.Subject == t.Subject {
			entered = &earlier[i].Transition
		}
	}
	closed := ClosedState{Transition: t, Entered: entered}
	if entered == nil {
		if !feature {
			return ClosedState{}, false
		}
		manifest, err := r.workstreamManifest(stream)
		if err != nil {
			return ClosedState{}, false
		}
		closed.Created = manifest.At
	}
	return closed, true
}

func finishedTurn(t Thread, response TurnResponse) (FinishedTurn, bool) {
	for _, q := range t.Turns {
		if q.Request.ID != response.RequestID || q.Request.Revision != response.RequestRevision {
			continue
		}
		turn := FinishedTurn{Request: q.Request, Response: response, Role: t.Identity.Role, Profile: responseProfile(q)}
		if q.Claim != nil {
			turn.ClaimedAt = q.Claim.At
		}
		return turn, true
	}
	return FinishedTurn{}, false
}

// WorkstreamCreated returns the time the workstream's trace was created, as its
// manifest records it.
func (r *Repository) WorkstreamCreated(stream config.WorkstreamID) (time.Time, error) {
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return time.Time{}, err
	}
	s, err := r.scoped(stream)
	if err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.workstreamManifest(stream)
	if err != nil {
		return time.Time{}, err
	}
	return m.At, nil
}
