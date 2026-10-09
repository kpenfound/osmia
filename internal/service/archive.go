package service

import (
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// archive permanently retires a terminal workstream, retaining its title.
func (s *Service) archive(raw string) (ArchiveResponse, *APIError) {
	if a, ok := s.archivedWorkstream(raw); ok {
		return ArchiveResponse{Project: a.Project, Workstream: a.Workstream, Archived: true}, nil
	}
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
	a, err := archiveTitle(repository, stream)
	if err != nil {
		return ArchiveResponse{}, &APIError{Internal, err.Error()}
	}
	a.ArchivedAt = s.now()
	if api := archiveError(s.store.SetArchived(a)); api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream, Archived: true}, nil
}

// unarchive refuses to reverse an archive, including one awaiting cleanup.
func (s *Service) unarchive(raw string) (ArchiveResponse, *APIError) {
	if _, ok := s.archivedWorkstream(raw); ok {
		return ArchiveResponse{}, &APIError{Conflict, "archive is permanent; the workstream cannot be restored"}
	}
	project, stream, _, api := s.conversationTrace(raw)
	if api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream}, nil
}

func (s *Service) archivedWorkstream(raw string) (runtime.Archive, bool) {
	if s.store == nil {
		return runtime.Archive{}, false
	}
	state, _ := s.store.Snapshot()
	for _, a := range state.Archived {
		if string(a.Workstream) == raw && s.current().Active(a.Project) {
			return a, true
		}
	}
	return runtime.Archive{}, false
}

func archiveTitle(repo *trace.Repository, stream config.WorkstreamID) (runtime.Archive, error) {
	list, err := repo.Statuses()
	if err != nil {
		return runtime.Archive{}, err
	}
	for _, w := range list {
		if w.Workstream != stream {
			continue
		}
		title := string(stream)
		if w.Status != nil && w.Status.Goal != "" {
			title = w.Status.Goal
		}
		return runtime.Archive{Project: repo.Project(), Workstream: stream, Title: title, State: w.State}, nil
	}
	return runtime.Archive{}, fmt.Errorf("cannot preserve title of workstream %s", stream)
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
