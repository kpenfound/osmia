package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// KindUnrecognisedSubject is the workflow subject that records the one notice
// a workstream of an unregistered kind raises to its manager.
const KindUnrecognisedSubject = "workstream-kind"

// KindUnrecognisedTransition identifies that notice's transition.
const KindUnrecognisedTransition = "kind-unrecognised"

// WorkstreamKind returns the registered kind of a workstream that is not a
// session, with the kind name its trace stores. ok is false when this build
// does not register the stored kind: that workstream waits and is never run as
// Feature.
func WorkstreamKind(repository *trace.Repository, stream config.WorkstreamID) (kind kinds.Kind, stored string, ok bool, err error) {
	stored, err = repository.Kind(stream)
	if err != nil {
		return kinds.Kind{}, "", false, err
	}
	kind, ok = kinds.Lookup(stored)
	return kind, stored, ok, nil
}

// KindRunnable reports whether this build runs a workstream: its stored kind
// is registered. A session is outside the registry and is not asked.
func KindRunnable(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	_, _, ok, err := WorkstreamKind(repository, stream)
	return ok, err
}

// ObservedWorkstreams lists the workstreams that are not sessions, whatever
// their kind: the workstreams whose records are read, accounted and told to
// their manager. NonSessionWorkstreams lists the subset that moves work
// and that waits when its kind is not registered.
func ObservedWorkstreams(repo *trace.Repository) ([]config.WorkstreamID, error) {
	return discoverWorkstreams(repo, nil, nil, true)
}

// KindUnrecognisedBody is the notice a workstream of an unregistered kind
// raises to its manager.
func KindUnrecognisedBody(stored string) string {
	return fmt.Sprintf("This workstream's kind %q is not registered by this build, so it waits and no work moves on it. It is never run as another kind. Registered kinds: %s. Run it with a build that registers its kind.", stored, strings.Join(kinds.Names(), ", "))
}

// KindUnrecognisedPass records, once per workstream, the notice that its kind
// is not registered. The transition is stamped with the workstream's creation
// time, so every later pass repeats the committed transition and raises no
// second notice.
func KindUnrecognisedPass(ctx context.Context, r *trace.Repository) error {
	streams, err := ObservedWorkstreams(r)
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, stored, ok, err := WorkstreamKind(r, stream)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		at, err := r.CreatedAt(stream)
		if err != nil {
			return err
		}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: KindUnrecognisedTransition, Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: ServiceActor, Cause: KindUnrecognisedTransition}
		tx := trace.Transaction{Transition: trace.Transition{Header: h, Subject: KindUnrecognisedSubject, To: "unrecognised", Reason: fmt.Sprintf("the workstream's kind %q is not registered by this build", stored)},
			Events: []trace.Event{trace.Notice(h.ID, "notice", KindUnrecognisedBody(stored))}}
		if _, err := r.Transact(ctx, tx); err != nil {
			return fmt.Errorf("unrecognised kind of %s: %w", stream, err)
		}
	}
	return nil
}

// CheckBaseKind refuses a base workstream of a kind that cannot be a base, and
// a base of a kind this build does not register. A base that does not exist
// is left to the checks that name it.
func (s *Controller) CheckBaseKind(project config.ProjectID, base config.WorkstreamID) *APIError {
	repository, err := s.Repository(project)
	if err != nil {
		return nil
	}
	streams, err := repository.Workstreams()
	if err != nil || !slices.Contains(streams, base) {
		return nil
	}
	return CheckDependencyKinds(repository, base)
}

// CheckDependencyKinds refuses a workstream of a kind whose flags forbid a
// base or dependency edge. A session, which is not a kind, is left to the
// checks that refuse it.
func CheckDependencyKinds(repository *trace.Repository, streams ...config.WorkstreamID) *APIError {
	for _, stream := range streams {
		kind, stored, ok, err := WorkstreamKind(repository, stream)
		if err != nil {
			return &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", stream)}
		}
		if stored == trace.SessionKind {
			continue
		}
		if !ok {
			return &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s has kind %q, which this build does not register", stream, stored)}
		}
		if err := kind.CheckDependencies(); err != nil {
			return &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s: %s", stream, err)}
		}
	}
	return nil
}
