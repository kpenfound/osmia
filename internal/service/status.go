package service

import (
	"fmt"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// statuses reports every workstream of the active project, or none when no
// project or trace is active. The librarian's workstream carries no feature
// and is left out. A workstream whose unit states or overlap advisories cannot
// be read is reported without them and its first such failure in unreadable,
// by workstream.
func (s *Service) statuses() ([]WorkstreamStatus, map[config.WorkstreamID]Diagnostic, *APIError) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	out := []WorkstreamStatus{}
	unreadable := map[config.WorkstreamID]Diagnostic{}
	if !cfg.HasProject() || active == nil {
		return out, unreadable, nil
	}
	list, err := active.repository.Statuses()
	if err != nil {
		return nil, nil, &APIError{Internal, fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", cfg.Project.ID)}
	}
	mode := s.Context().Mode(cfg.Project.ID)
	librarian := librarianWorkstream(cfg.Project.ID)
	for _, w := range list {
		if w.Workstream == librarian {
			continue
		}
		view := statusView(cfg.Project.ID, mode, w)
		units, err := unitStates(active.repository, w.Workstream, w.Subjects)
		if err != nil {
			unreadable[w.Workstream] = Diagnostic{"units", Internal, fmt.Sprintf("cannot read the unit states of workstream %s; check the trace repository", w.Workstream)}
		}
		view.Units = append(view.Units, units...)
		advisories, err := overlapAdvisories(active.repository, w.Workstream, w.Subjects)
		if err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{"advisories", Internal, fmt.Sprintf("cannot read the overlap advisories of workstream %s; check the trace repository", w.Workstream)}
			}
		} else {
			view.Advisories = advisories
		}
		for i := range view.Units {
			for _, gate := range view.Gates {
				if gate.Kind == UnitContested && gate.Reference == view.Units[i].Unit {
					view.Units[i].Reason = gate.Reason
				}
			}
		}
		out = append(out, view)
	}
	return out, unreadable, nil
}

func (s *Service) statusList() StatusResponse {
	list, unreadable, api := s.statuses()
	if api != nil {
		return StatusResponse{Workstreams: []WorkstreamStatus{}, Diagnostics: []Diagnostic{{"workstreams", api.Code, api.Message}}}
	}
	diagnostics := []Diagnostic{}
	for _, w := range list {
		if d, ok := unreadable[w.Workstream]; ok {
			diagnostics = append(diagnostics, d)
		}
	}
	return StatusResponse{Workstreams: list, Diagnostics: diagnostics}
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
	list, unreadable, api := s.statuses()
	if api != nil {
		return WorkstreamStatus{}, api
	}
	if d, ok := unreadable[id]; ok {
		return WorkstreamStatus{}, &APIError{d.Code, d.Message}
	}
	for _, w := range list {
		if w.Workstream == id {
			return w, nil
		}
	}
	return WorkstreamStatus{}, &APIError{NotFound, fmt.Sprintf("workstream %s is not in the active project; list workstreams with osmia status", id)}
}

func statusView(project config.ProjectID, mode bundle.Mode, w trace.WorkstreamStatus) WorkstreamStatus {
	out := WorkstreamStatus{Workstream: w.Workstream, Project: project, Units: []UnitStatus{}, Advisories: []OverlapAdvisory{}, OpenQuestions: w.OpenQuestions, Gates: append([]trace.OwnerGate{}, w.Gates...), ContextMode: mode}
	if w.State != "" {
		state := w.State
		out.State = &state
	}
	if st := w.Status; st != nil {
		out.Status = &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}
	}
	return out
}
