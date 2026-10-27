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
		_, c, err := s.LoadCharter(ctx, project)
		if errors.Is(err, ErrNoTrace) {
			return &APIError{Code: Internal, Message: fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", project)}
		}
		if err != nil {
			return &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the charter of project %s; check the project trace", project)}
		}
		if c.Empty() {
			return &APIError{Code: CharterEmpty, Message: fmt.Sprintf("project %s cannot take work: its charter has no rules; write numbered rules (\"1. ...\") with: osmia edit charter %s", project, project)}
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
			if base != LibrarianWorkstream(project) && slices.Contains(streams, base) {
				return project, nil
			}
		}
	}
	return "", &APIError{Code: Validation, Message: "base must name another feature workstream in a selected project"}
}

// RecordOwnerProjects pins member identities before the handed input is durable.
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

// ChooseDesignProjects answers only an unselected handed design from an upgrade.
func (s *Controller) ChooseDesignProjects(ctx context.Context, raw string, projects []config.ProjectID) (DesignStartResponse, *APIError) {
	project, stream, r, api := s.ConversationTrace(raw)
	if api != nil {
		return DesignStartResponse{}, api
	}
	lock := DesignLock(r, stream)
	lock.Lock()
	defer lock.Unlock()
	_, found, err := DesignSelectionOf(r, stream)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if found {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "this start already has its owner-selected projects"}
	}
	feature, err := r.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if feature.Value != "" && feature.Value != HandedState {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "this design start is no longer awaiting projects"}
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	var handed trace.Document
	for _, d := range docs {
		if d.ID == HandedDocument && d.Source == "design:"+string(stream) {
			handed = d
		}
	}
	if handed.Revision == 0 {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "this is not an unselected handed design start"}
	}
	base, err := r.WorkstreamBase(stream)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if base.Base != "" && !slices.Contains(projects, project) {
		return DesignStartResponse{}, &APIError{Code: Validation, Message: "select the project containing the design's base"}
	}
	if api := s.RecordOwnerProjects(ctx, r, stream, projects, base.Base); api != nil {
		return DesignStartResponse{}, api
	}
	if err := s.ResumeHandedDesign(ctx, r, stream, handed); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if err := s.ReconcileDesignStart(ctx, r, stream); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	return DesignStartResponse{Project: project, Workstream: stream, State: HandedState, Handed: handed.Path, Source: handed.Source}, nil
}

// LegacyProjectsEntry exposes an owner decision for a handed, unselected design.
func (s *Controller) LegacyProjectsEntry(r *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	if _, found, err := DesignSelectionOf(r, stream); err != nil || found {
		return InboxEntry{}, false, err
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	feature, err := r.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value == AbandonedState {
		return InboxEntry{}, false, err
	}
	for _, d := range docs {
		if d.ID != HandedDocument || d.Source != "design:"+string(stream) {
			continue
		}
		e := Decision("project-selection", stream, d.At, "/design/"+string(stream)+"/projects", map[string]any{"projects": []config.ProjectID{}})
		e.Question = "Choose the projects for this handed design start. The first project is home for the members; the design conversation stays here."
		e.Blocked = "Allocating and drafting the workgroup."
		return e, true, nil
	}
	return InboxEntry{}, false, nil
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
			// Allocation derives each member from its project and origin.
			// Locate that origin without reading unrelated member traces,
			// which may be held at a retirement snapshot boundary.
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
