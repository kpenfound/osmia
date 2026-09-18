package service

import (
	"fmt"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// statuses reports every workstream of the active project, or none when no
// project or trace is active. The librarian's workstream carries no feature
// and is left out.
func (s *Service) statuses() ([]WorkstreamStatus, *APIError) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	out := []WorkstreamStatus{}
	if !cfg.HasProject() || active == nil {
		return out, nil
	}
	list, err := active.repository.Statuses()
	if err != nil {
		return nil, &APIError{Internal, fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", cfg.Project.ID)}
	}
	mode := s.Context().Mode(cfg.Project.ID)
	librarian := librarianWorkstream(cfg.Project.ID)
	for _, w := range list {
		if w.Workstream == librarian {
			continue
		}
		units, err := unitStates(active.repository, w.Workstream)
		if err != nil {
			return nil, &APIError{Internal, fmt.Sprintf("cannot read the unit states of workstream %s; check the trace repository", w.Workstream)}
		}
		view := statusView(cfg.Project.ID, mode, w)
		view.Units = append(view.Units, units...)
		out = append(out, view)
	}
	return out, nil
}

func (s *Service) statusList() StatusResponse {
	list, api := s.statuses()
	if api != nil {
		return StatusResponse{Workstreams: []WorkstreamStatus{}, Diagnostics: []Diagnostic{{"workstreams", api.Code, api.Message}}}
	}
	return StatusResponse{Workstreams: list, Diagnostics: []Diagnostic{}}
}

// workstreamStatus reports one workstream of the active project.
func (s *Service) workstreamStatus(raw string) (WorkstreamStatus, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return WorkstreamStatus{}, &APIError{Validation, "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	if !s.current().HasProject() {
		return WorkstreamStatus{}, &APIError{NoProject, "no project is configured; add one with osmia project add"}
	}
	list, api := s.statuses()
	if api != nil {
		return WorkstreamStatus{}, api
	}
	for _, w := range list {
		if w.Workstream == id {
			return w, nil
		}
	}
	return WorkstreamStatus{}, &APIError{NotFound, fmt.Sprintf("workstream %s is not in the active project; list workstreams with osmia status", id)}
}

func statusView(project config.ProjectID, mode bundle.Mode, w trace.WorkstreamStatus) WorkstreamStatus {
	out := WorkstreamStatus{Workstream: w.Workstream, Project: project, Units: []UnitStatus{}, OpenQuestions: w.OpenQuestions, ContextMode: mode}
	if w.State != "" {
		state := w.State
		out.State = &state
	}
	if st := w.Status; st != nil {
		out.Status = &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}
	}
	return out
}
