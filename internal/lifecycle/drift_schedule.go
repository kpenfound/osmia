package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// DriftRequestSubject is the workflow subject that tracks the owner's
// requests for a workstream's drift rebases: requested-<k> once the owner
// asks for a drift rebase that drift rebase k answers.
const DriftRequestSubject = "drift-request"

// DriftHistory is what a workstream's next drift rebase is measured from.
type DriftHistory struct {
	// Drift is the number of the workstream's latest drift rebase, and
	// Requested the number of the drift rebase the latest request waits
	// for; each is 0 before there is one.
	Drift, Requested int
	// RequestedAt is when the latest request was recorded, and RequestedBy
	// who made it: the owner, or the chief of staff handing a held drift
	// rebase back.
	RequestedAt time.Time
	RequestedBy string
	// Held reports whether the latest drift rebase is held for the owner.
	Held bool
	// Since is when the latest drift rebase, final rebase or the sealing
	// that created the feature branch was recorded, and What names it.
	Since time.Time
	What  string
}

// History reads the workstream's drift history from its trace.
func (f *Foreman) History(stream config.WorkstreamID) (DriftHistory, error) {
	records, err := trace.Read[trace.Record](f.repository, stream)
	if err != nil {
		return DriftHistory{}, err
	}
	var h DriftHistory
	var drift, request, final, sealed time.Time
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			switch r.Subject {
			case DriftSubject:
				if !r.At.Before(drift) {
					drift = r.At
				}
			case DriftRequestSubject:
				if !r.At.Before(request) {
					request, h.RequestedBy = r.At, rulerName(r.Actor)
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
		return DriftHistory{}, err
	}
	if h.Drift, err = DriftNumber(states[DriftSubject].Value); err != nil {
		return DriftHistory{}, err
	}
	if h.Requested, err = DriftNumber(states[DriftRequestSubject].Value); err != nil {
		return DriftHistory{}, err
	}
	h.Held = strings.HasPrefix(states[DriftSubject].Value, DriftHeld+"-")
	h.RequestedAt = request
	for _, at := range []struct {
		T    time.Time
		What string
	}{{sealed, "sealing"}, {final, "final rebase"}, {drift, fmt.Sprintf("drift rebase %d", h.Drift)}} {
		if !at.T.IsZero() && !at.T.Before(h.Since) {
			h.Since, h.What = at.T, at.What
		}
	}
	return h, nil
}

// DriftDue returns why the sealed workstream takes a drift rebase at now, or
// "" when it takes none: the owner, or the chief of staff handing a held one
// back, asked for one it has not had, or, unless
// its latest drift rebase is held for the owner, its base workstream's
// branch moved or the project's upstream_rebase interval has elapsed since
// its latest drift rebase, final rebase or, before either, its sealing.
func (f *Foreman) DriftDue(stream config.WorkstreamID, now time.Time) (string, error) {
	return f.driftDueContext(context.Background(), stream, now)
}

func (f *Foreman) driftDueContext(ctx context.Context, stream config.WorkstreamID, now time.Time) (string, error) {
	h, err := f.History(stream)
	if err != nil {
		return "", err
	}
	if h.Held && h.Requested <= h.Drift {
		return "", nil
	}
	feature, err := f.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	if feature.Value == DeliveredState {
		if h.Requested > h.Drift {
			return fmt.Sprintf("the %s asked for drift rebase %d", h.RequestedBy, h.Requested), nil
		}
		return "", nil
	}
	if changed, err := DependentBaseChanged(ctx, f.cfg, f.repository, stream); err != nil || changed {
		return "the base workstream branch moved", err
	}
	if h.Since.IsZero() {
		return "", nil
	}
	if h.Requested > h.Drift {
		return fmt.Sprintf("the %s asked for a drift rebase at %s", h.RequestedBy, h.RequestedAt.Format(time.RFC3339)), nil
	}
	interval := f.cfg.Project.RebaseInterval()
	if interval <= 0 || now.Sub(h.Since) < interval {
		return "", nil
	}
	return fmt.Sprintf("upstream_rebase %s has elapsed since the %s at %s", interval, h.What, h.Since.Format(time.RFC3339)), nil
}

// DriftCheck is how often the foreman reads the drift schedule: the
// shortest upstream_rebase interval.
const DriftCheck = time.Minute

// Drifts asks for the drift rebases that are due on the project's lander.
// It reads the schedule at most once per driftCheck, and at the next pass
// after an owner's request. The trace holds everything the schedule is
// measured from, so a restart neither resets an elapsed interval nor asks
// for one twice.
func (f *Foreman) Drifts(ctx context.Context) error {
	now := f.s.Now()
	if !f.s.DriftAsked.Swap(false) && now.Before(f.NextDrift) {
		return nil
	}
	f.NextDrift = now.Add(DriftCheck)
	_, err := f.RequestDrifts(ctx, func(stream config.WorkstreamID) (string, error) {
		return f.driftDueContext(ctx, stream, now)
	})
	return err
}

// AskDrift records the owner's request for a drift rebase of the
// workstream and returns the number of the drift rebase that answers it:
// the next one the workstream has not been asked for. A request already
// waiting for that drift rebase is kept as it is.
func (f *Foreman) AskDrift(ctx context.Context, stream config.WorkstreamID, at time.Time) (int, error) {
	states, err := f.repository.WorkflowStates(stream)
	if err != nil {
		return 0, err
	}
	drift, err := DriftNumber(states[DriftSubject].Value)
	if err != nil {
		return 0, err
	}
	k := drift + 1
	current := states[DriftRequestSubject]
	if waiting, err := DriftNumber(current.Value); err != nil || waiting >= k {
		return k, err
	}
	id := fmt.Sprintf("%s-%d", DriftRequestSubject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repository.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: DriftRequestSubject}
	reason := fmt.Sprintf("the owner asked for a drift rebase of %s onto %s of %s; drift rebase %d answers it on the project's lander", featureBranch(stream), f.cfg.Project.BaseBranch, f.cfg.Project.Upstream, k)
	tx := trace.Transaction{ExpectedVersion: current.Version, Transition: trace.Transition{Header: h, Subject: DriftRequestSubject, From: current.Value, To: fmt.Sprintf("requested-%d", k), Reason: reason}}
	if _, err := f.repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if state, readErr := f.repository.Workflow(stream, DriftRequestSubject); readErr == nil {
				if waiting, _ := DriftNumber(state.Value); waiting >= k {
					return k, nil
				}
			}
		}
		return 0, err
	}
	return k, nil
}

// RebaseProject records the owner's request for a drift rebase of every
// Eligible workstream of the project that is not paused; the project may be
// left out while exactly one is active. The foreman asks for each on the
// Project's lander, whether or not
// scheduled drift rebases are enabled. It returns once every request is
// durable, with the workstreams the request covers and those it skips.
func (s *Controller) RebaseProject(ctx context.Context, req ProjectRebaseRequest) (ProjectRebaseResponse, *APIError) {
	id, api := s.ProjectFor(req.Project)
	if api != nil {
		return ProjectRebaseResponse{}, api
	}
	req.Project = id
	cfg, active := s.RuntimeOf(id)
	if !cfg.HasProject() {
		return ProjectRebaseResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	if active == nil {
		return ProjectRebaseResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the drift rebase request in the trace of project %s; check the trace repository", req.Project)}
	streams, err := active.Repository.Workstreams()
	if err != nil {
		return ProjectRebaseResponse{}, failed
	}
	d := Drifter{&Foreman{masons: &masons{s: s, cfg: cfg, repository: active.Repository}}}
	out := ProjectRebaseResponse{Project: cfg.Project.ID, Covered: []DriftCoverage{}, Skipped: []DriftSkip{}}
	librarian := librarianWorkstream(cfg.Project.ID)
	at := s.Now()
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		reason, err := d.Eligible(stream)
		if err != nil {
			return ProjectRebaseResponse{}, failed
		}
		if reason != "" {
			out.Skipped = append(out.Skipped, DriftSkip{Workstream: stream, Reason: reason})
			continue
		}
		k, err := d.AskDrift(ctx, stream, at)
		if err != nil {
			return ProjectRebaseResponse{}, failed
		}
		out.Covered = append(out.Covered, DriftCoverage{Workstream: stream, Drift: k})
	}
	if len(out.Covered) > 0 {
		s.DriftAsked.Store(true)
	}
	return out, nil
}

// RebaseWorkstream records the owner's request for a drift rebase of the
// Eligible, unpaused workstream named by raw. The foreman asks for it on the project's lander, whether or not
// scheduled drift rebases are enabled, and a held drift rebase takes it as
// the owner asking for another. It returns once the request is durable,
// with the number of the drift rebase that answers it.
func (s *Controller) RebaseWorkstream(ctx context.Context, raw string) (WorkstreamRebaseResponse, *APIError) {
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return WorkstreamRebaseResponse{}, api
	}
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the drift rebase request in the trace of workstream %s; check the trace repository", stream)}
	d := Drifter{&Foreman{masons: &masons{s: s, cfg: s.About(repository), repository: repository}}}
	reason, err := d.Eligible(stream)
	if err != nil {
		return WorkstreamRebaseResponse{}, failed
	}
	if reason != "" {
		return WorkstreamRebaseResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s takes no drift rebase: %s", stream, reason)}
	}
	k, err := d.AskDrift(ctx, stream, s.Now())
	if err != nil {
		return WorkstreamRebaseResponse{}, failed
	}
	s.DriftAsked.Store(true)
	return WorkstreamRebaseResponse{Project: project, Workstream: stream, Drift: k}, nil
}

// LatestDrift returns the workstream's latest drift rebase as status reports
// it, with what its upstream moved events told the chief of staff, or nil
// before its first.
func LatestDrift(repository *trace.Repository, stream config.WorkstreamID, states map[string]trace.WorkflowState) (*DriftStatus, error) {
	value := states[DriftSubject].Value
	if value == "" {
		return nil, nil
	}
	k, err := DriftNumber(value)
	if err != nil {
		return nil, err
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return nil, err
	}
	outcome, _, _ := strings.Cut(value, "-")
	out := &DriftStatus{Drift: k, Outcome: outcome, Moved: []string{}}
	for _, t := range transitions {
		if t.Subject == DriftSubject && t.To == value && !t.At.Before(out.At) {
			out.At, out.Reason = t.At, t.Reason
		}
	}
	entries, err := repository.Outbox(stream)
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(entries, func(e trace.OutboxEntry) bool {
		return e.Event.Kind != trace.UpstreamMovedKind || e.Event.ID != trace.EventID(e.TransitionID, trace.UpstreamMovedKey(k))
	})
	slices.SortStableFunc(entries, func(a, b trace.OutboxEntry) int {
		return a.At.Compare(b.At)
	})
	for _, e := range entries {
		out.Moved = append(out.Moved, e.Event.Body)
	}
	return out, nil
}
