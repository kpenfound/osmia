package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ValidateProjects checks an owner-chosen ordered list without recording anything.
// Paused projects remain active and may be selected.
func (s *Controller) ValidateProjects(ctx context.Context, projects []config.ProjectID) *APIError {
	if len(projects) == 0 {
		return &APIError{Code: Validation, Message: "select at least one project"}
	}
	seen := map[config.ProjectID]bool{}
	for _, project := range projects {
		if config.CheckProjectIDs(project) != nil || seen[project] {
			return &APIError{Code: Validation, Message: fmt.Sprintf("invalid or duplicate project %s", project)}
		}
		seen[project] = true
		if !s.Current().Active(project) {
			return &APIError{Code: Validation, Message: fmt.Sprintf("project %s is not active", project)}
		}
		_, _, err := s.LoadCharter(ctx, project)
		if errors.Is(err, ErrNoTrace) {
			return &APIError{Code: Internal, Message: fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", project)}
		}
		if err != nil {
			return &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the charter of project %s; check the project trace", project)}
		}
	}
	return nil
}

// SelectionBase resolves a prerequisite to its selected project before allocation.
func (s *Controller) SelectionBase(projects []config.ProjectID, base config.WorkstreamID) (config.ProjectID, *APIError) {
	if base == "" {
		return "", nil
	}
	if _, archived := s.ArchivedWorkstream(string(base)); !archived {
		for _, project := range projects {
			r, err := s.Repository(project)
			if err != nil {
				return "", &APIError{Code: Internal, Message: err.Error()}
			}
			streams, err := r.Workstreams()
			if err != nil {
				return "", &APIError{Code: Internal, Message: err.Error()}
			}
			if slices.Contains(streams, base) {
				return project, nil
			}
		}
	}
	return "", &APIError{Code: Validation, Message: "base must name another feature workstream in a selected project"}
}

// RecordOwnerProjects records the owner selection and stable member identities.
// The conversation may be hosted outside the selected projects. A recorded list
// is immutable; recovery reads it without consulting a model or current briefs.
func (s *Controller) RecordOwnerProjects(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, projects []config.ProjectID, base config.WorkstreamID) *APIError {
	if api := s.ValidateProjects(ctx, projects); api != nil {
		return api
	}
	baseProject, api := s.SelectionBase(projects, base)
	if api != nil {
		return api
	}
	selected := DesignSelection{Group: config.WorkgroupID("g_" + strings.TrimPrefix(string(stream), "w_")), Base: base, BaseProject: baseProject}
	for _, project := range projects {
		member := HandInWorkstream(project, "design-"+string(stream))
		if len(projects) == 1 && project == r.Project() {
			member = stream
		}
		selected.Members = append(selected.Members, DesignMember{Repo: project, Workstream: member})
	}
	old, found, err := DesignSelectionOf(r, stream)
	if err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	data, _ := json.Marshal(selected)
	if found {
		prior, _ := json.Marshal(old)
		if string(prior) != string(data) {
			return &APIError{Code: Conflict, Message: "the owner's project selection is already recorded"}
		}
		return nil
	}
	h := OwnerHeader(DesignSelectionDocument, r.Project(), stream, DesignStartTransition, s.Now())
	h.Schema = "osmia.trace.document"
	if err := r.RecordDocuments(ctx, []trace.Document{{Header: h, Path: DesignSelectionPath, Content: string(data)}}); err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	if err := s.Step("design-selection-recorded"); err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	return nil
}

// ChooseDesignProjects refuses to change the projects of a design start: a
// start always pins its projects when it is made.
func (s *Controller) ChooseDesignProjects(_ context.Context, raw string, _ []config.ProjectID) (DesignStartResponse, *APIError) {
	_, stream, r, api := s.ConversationTrace(raw)
	if api != nil {
		return DesignStartResponse{}, api
	}
	if _, found, err := DesignSelectionOf(r, stream); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	} else if found {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "this start already has its owner-selected projects"}
	}
	return DesignStartResponse{}, &APIError{Code: Conflict, Message: "this start already has its pinned projects"}
}

// SelectionJoiningMember holds individual abandonment during allocation and
// join, before every member has a workgroup locator for the ratification gate.
func (s *Controller) SelectionJoiningMember(stream config.WorkstreamID) (bool, error) {
	_, projects := s.Runtimes()
	for _, project := range projects {
		streams, err := project.Repository.Workstreams()
		if err != nil {
			return false, err
		}
		for _, origin := range streams {
			if origin != stream && !slices.ContainsFunc(projects, func(p *Project) bool {
				return HandInWorkstream(p.ID, "design-"+string(origin)) == stream
			}) {
				continue
			}
			selected, found, err := DesignSelectionOf(project.Repository, origin)
			if err != nil {
				return false, err
			}
			if !found || len(selected.Members) < 2 {
				continue
			}
			if origin != stream && !slices.ContainsFunc(selected.Members, func(m DesignMember) bool { return m.Workstream == stream }) {
				continue
			}
			state, err := project.Repository.Workflow(origin, DesignStartSubject)
			return state.Value != "complete", err
		}
	}
	return false, nil
}
