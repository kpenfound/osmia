package service

import (
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// archive records a delivered or abandoned workstream as archived, which
// takes it out of the owner's list of work. Nothing is deleted: the trace,
// the handed inputs and any branch stay, and unarchive returns it to the
// list. Archiving an archived workstream changes nothing.
func (s *Service) archive(raw string) (ArchiveResponse, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ArchiveResponse{}, api
	}
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return ArchiveResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the state of workstream %s; check the trace repository", stream)}
	}
	if state.Value != DeliveredState && state.Value != AbandonedState {
		current := state.Value
		if current == "" {
			current = "handed"
		}
		return ArchiveResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is %s; only a delivered or abandoned workstream can be archived, so abandon it first", stream, current)}
	}
	if api := archiveError(s.store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: s.now()})); api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream, Archived: true}, nil
}

// unarchive returns an archived workstream to the owner's list of work.
// Unarchiving a workstream that is not archived changes nothing.
func (s *Service) unarchive(raw string) (ArchiveResponse, *APIError) {
	project, stream, _, api := s.conversationTrace(raw)
	if api != nil {
		return ArchiveResponse{}, api
	}
	if api := archiveError(s.store.ClearArchived(stream)); api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream, Archived: false}, nil
}

func archiveError(err error) *APIError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrValidation):
		return &APIError{Validation, "the runtime settings refused the change: " + err.Error()}
	case errors.Is(err, runtime.ErrConflict):
		return &APIError{Conflict, "runtime file changed externally; restore it or restart the service"}
	}
	return &APIError{Internal, "cannot write the runtime settings; check the root's runtime.json"}
}

// archivedIn reports whether the runtime state in force archives stream.
func archivedIn(state runtime.State, stream config.WorkstreamID) bool {
	return slices.ContainsFunc(state.Archived, func(a runtime.Archive) bool { return a.Workstream == stream })
}
