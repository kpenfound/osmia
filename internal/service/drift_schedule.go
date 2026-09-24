package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// driftRequestSubject is the workflow subject that tracks the owner's
// requests for a workstream's drift rebases: requested-<k> once the owner
// asks for a drift rebase that drift rebase k answers.
const driftRequestSubject = "drift-request"

// driftHistory is what a workstream's next drift rebase is measured from.
type driftHistory struct {
	// Drift is the number of the workstream's latest drift rebase, and
	// Requested the number of the drift rebase the owner's latest request
	// waits for; each is 0 before there is one.
	Drift, Requested int
	// RequestedAt is when the owner's latest request was recorded.
	RequestedAt time.Time
	// Since is when the latest drift rebase, final rebase or the sealing
	// that created the feature branch was recorded, and What names it.
	Since time.Time
	What  string
}

// history reads the workstream's drift history from its trace.
func (f *foreman) history(stream config.WorkstreamID) (driftHistory, error) {
	records, err := trace.Read[trace.Record](f.repository, stream)
	if err != nil {
		return driftHistory{}, err
	}
	var h driftHistory
	var drift, request, final, sealed time.Time
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			switch r.Subject {
			case driftSubject:
				if !r.At.Before(drift) {
					drift = r.At
				}
			case driftRequestSubject:
				if !r.At.Before(request) {
					request = r.At
				}
			}
		case trace.Document:
			switch r.ID {
			case finalRebaseDocument:
				if r.At.After(final) {
					final = r.At
				}
			case seal.DocumentID:
				if sealed.IsZero() || r.At.Before(sealed) {
					sealed = r.At
				}
			}
		}
	}
	states, err := f.repository.WorkflowStates(stream)
	if err != nil {
		return driftHistory{}, err
	}
	if h.Drift, err = driftNumber(states[driftSubject].Value); err != nil {
		return driftHistory{}, err
	}
	if h.Requested, err = driftNumber(states[driftRequestSubject].Value); err != nil {
		return driftHistory{}, err
	}
	h.RequestedAt = request
	for _, at := range []struct {
		t    time.Time
		what string
	}{{sealed, "sealing"}, {final, "final rebase"}, {drift, fmt.Sprintf("drift rebase %d", h.Drift)}} {
		if !at.t.IsZero() && !at.t.Before(h.Since) {
			h.Since, h.What = at.t, at.what
		}
	}
	return h, nil
}

// driftDue returns why the sealed workstream takes a drift rebase at now, or
// "" when it takes none: the owner asked for one it has not had, or the
// project's upstream_rebase interval has elapsed since its latest drift
// rebase, final rebase or, before either, its sealing.
func (f *foreman) driftDue(stream config.WorkstreamID, now time.Time) (string, error) {
	h, err := f.history(stream)
	if err != nil || h.Since.IsZero() {
		return "", err
	}
	if h.Requested > h.Drift {
		return fmt.Sprintf("the owner asked for a drift rebase at %s", h.RequestedAt.Format(time.RFC3339)), nil
	}
	interval := f.cfg.Project.RebaseInterval()
	if interval <= 0 || now.Sub(h.Since) < interval {
		return "", nil
	}
	return fmt.Sprintf("upstream_rebase %s has elapsed since the %s at %s", interval, h.What, h.Since.Format(time.RFC3339)), nil
}

// driftCheck is how often the foreman reads the drift schedule: the
// shortest upstream_rebase interval.
const driftCheck = time.Minute

// drifts asks for the drift rebases that are due on the project's lander.
// It reads the schedule at most once per driftCheck, and at the next pass
// after an owner's request. The trace holds everything the schedule is
// measured from, so a restart neither resets an elapsed interval nor asks
// for one twice.
func (f *foreman) drifts(ctx context.Context) error {
	now := f.s.now()
	if !f.s.driftAsked.Swap(false) && now.Before(f.nextDrift) {
		return nil
	}
	f.nextDrift = now.Add(driftCheck)
	_, err := f.requestDrifts(ctx, func(stream config.WorkstreamID) (string, error) { return f.driftDue(stream, now) })
	return err
}

// askDrift records the owner's request for a drift rebase of the
// workstream and returns the number of the drift rebase that answers it:
// the next one the workstream has not been asked for. A request already
// waiting for that drift rebase is kept as it is.
func (f *foreman) askDrift(ctx context.Context, stream config.WorkstreamID, at time.Time) (int, error) {
	states, err := f.repository.WorkflowStates(stream)
	if err != nil {
		return 0, err
	}
	drift, err := driftNumber(states[driftSubject].Value)
	if err != nil {
		return 0, err
	}
	k := drift + 1
	current := states[driftRequestSubject]
	if waiting, err := driftNumber(current.Value); err != nil || waiting >= k {
		return k, err
	}
	id := fmt.Sprintf("%s-%d", driftRequestSubject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repository.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: driftRequestSubject}
	reason := fmt.Sprintf("the owner asked for a drift rebase of %s onto %s of %s; drift rebase %d answers it on the project's lander", featureBranch(stream), f.cfg.Project.BaseBranch, f.cfg.Project.Upstream, k)
	tx := trace.Transaction{ExpectedVersion: current.Version, Transition: trace.Transition{Header: h, Subject: driftRequestSubject, From: current.Value, To: fmt.Sprintf("requested-%d", k), Reason: reason}}
	if _, err := f.repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if state, readErr := f.repository.Workflow(stream, driftRequestSubject); readErr == nil {
				if waiting, _ := driftNumber(state.Value); waiting >= k {
					return k, nil
				}
			}
		}
		return 0, err
	}
	return k, nil
}

// rebaseProject records the owner's request for a drift rebase of every
// building or assembled workstream of the active project that is not
// paused. The foreman asks for each on the project's lander, whether or not
// scheduled drift rebases are enabled. It returns once every request is
// durable, with the workstreams the request covers and those it skips.
func (s *Service) rebaseProject(ctx context.Context, req ProjectRebaseRequest) (ProjectRebaseResponse, *APIError) {
	if err := config.CheckProjectIDs(req.Project); err != nil {
		return ProjectRebaseResponse{}, &APIError{Validation, "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() || cfg.Project.ID != req.Project {
		return ProjectRebaseResponse{}, &APIError{NotFound, fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	if active == nil {
		return ProjectRebaseResponse{}, &APIError{Internal, fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot record the drift rebase request in the trace of project %s; check the trace repository", req.Project)}
	streams, err := active.repository.Workstreams()
	if err != nil {
		return ProjectRebaseResponse{}, failed
	}
	d := drifter{&foreman{masons: &masons{s: s, cfg: cfg, repository: active.repository}}}
	out := ProjectRebaseResponse{Project: cfg.Project.ID, Covered: []DriftCoverage{}, Skipped: []DriftSkip{}}
	librarian := librarianWorkstream(cfg.Project.ID)
	at := s.now()
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		reason, err := d.eligible(stream)
		if err != nil {
			return ProjectRebaseResponse{}, failed
		}
		if reason != "" {
			out.Skipped = append(out.Skipped, DriftSkip{Workstream: stream, Reason: reason})
			continue
		}
		k, err := d.askDrift(ctx, stream, at)
		if err != nil {
			return ProjectRebaseResponse{}, failed
		}
		out.Covered = append(out.Covered, DriftCoverage{Workstream: stream, Drift: k})
	}
	if len(out.Covered) > 0 {
		s.driftAsked.Store(true)
	}
	return out, nil
}

// latestDrift returns the workstream's latest drift rebase as status reports
// it, or nil before its first.
func latestDrift(repository *trace.Repository, stream config.WorkstreamID, states map[string]trace.WorkflowState) (*DriftStatus, error) {
	value := states[driftSubject].Value
	if value == "" {
		return nil, nil
	}
	k, err := driftNumber(value)
	if err != nil {
		return nil, err
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return nil, err
	}
	outcome, _, _ := strings.Cut(value, "-")
	out := &DriftStatus{Drift: k, Outcome: outcome}
	for _, t := range transitions {
		if t.Subject == driftSubject && t.To == value && !t.At.Before(out.At) {
			out.At, out.Reason = t.At, t.Reason
		}
	}
	return out, nil
}
