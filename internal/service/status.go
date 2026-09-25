package service

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// statuses reports every workstream of the active project, or none when no
// project or trace is active. The librarian's workstream carries no feature
// and is left out. A workstream whose agent turns, unit states, overlap
// advisories or drift rebases cannot be read is reported without them and its
// first such failure in unreadable, by workstream.
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
		agents, err := agentStatuses(active.repository, w.Workstream, time.Now())
		if err == nil {
			err = s.step("status-agents")
		}
		if err != nil {
			unreadable[w.Workstream] = Diagnostic{"agents", Internal, fmt.Sprintf("cannot read the agent turns of workstream %s; check the trace repository", w.Workstream)}
		} else {
			view.Agents = agents
		}
		units, err := unitStates(active.repository, w.Workstream, w.Subjects)
		if err == nil {
			err = s.step("status-units")
		}
		if err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{"units", Internal, fmt.Sprintf("cannot read the unit states of workstream %s; check the trace repository", w.Workstream)}
			}
			units = nil
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
		drift, err := latestDrift(active.repository, w.Workstream, w.Subjects)
		if err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{"drift", Internal, fmt.Sprintf("cannot read the drift rebases of workstream %s; check the trace repository", w.Workstream)}
			}
		} else {
			view.Drift = drift
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

func agentStatuses(repository *trace.Repository, stream config.WorkstreamID, now time.Time) ([]AgentStatus, error) {
	threads, err := repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	agents := []AgentStatus{}
	questionIDs := map[[3]string]string{}
	for _, thread := range threads {
		if thread.Parked() {
			questions, err := repository.Questions(stream)
			if err != nil {
				return nil, err
			}
			for _, q := range questions {
				a := q.Asked
				questionIDs[[3]string{a.AskedBy.ID, a.Thread, a.Turn}] = a.ID
			}
			break
		}
	}
	for _, thread := range threads {
		if thread.Active == "" && !thread.Parked() {
			continue
		}
		for _, turn := range thread.Turns {
			if turn.Request.TurnID != thread.Active && (!thread.Parked() || turn.Sequence != uint64(len(thread.Turns))) {
				continue
			}
			if turn.Claim == nil {
				continue
			}
			end := now
			if turn.Response != nil {
				end = turn.Response.At
			}
			entry := AgentStatus{Role: thread.Identity.Role, Unit: string(turn.Request.Unit), State: thread.Status, StartedAt: turn.Claim.At,
				Elapsed: max(0, int64(end.Sub(turn.Claim.At)/time.Second)), Profile: turn.Request.Profile.Name}
			if thread.Parked() {
				entry.State = "waiting"
				entry.QuestionID = questionIDs[[3]string{thread.Identity.ID, turn.Request.ThreadID, turn.Request.TurnID}]
			}
			if n := len(turn.Attempts); n > 0 {
				a := turn.Attempts[n-1]
				entry.Profile, entry.Attempt, entry.Path = a.Profile.Name, a.Number, a.Path
			}
			agents = append(agents, entry)
		}
	}
	return agents, nil
}

func (s *Service) statusList() StatusResponse {
	state, _ := s.effective()
	profiles := s.effectiveProfiles(state)
	budget, unread := s.dailyBudgetStatus()
	diagnostics := []Diagnostic{}
	if unread != nil {
		diagnostics = append(diagnostics, *unread)
	}
	streaks, err := s.failureStreaks()
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{"failure_streaks", Internal, "cannot read the turn attempts of the active project; check the trace repository"})
	}
	list, unreadable, api := s.statuses()
	if api != nil {
		return StatusResponse{Workstreams: []WorkstreamStatus{}, Profiles: profiles, ProviderLimits: state.ProviderLimits, DailyBudget: budget, FailureStreaks: streaks, Diagnostics: append(diagnostics, Diagnostic{"workstreams", api.Code, api.Message})}
	}
	for _, w := range list {
		if d, ok := unreadable[w.Workstream]; ok {
			diagnostics = append(diagnostics, d)
		}
	}
	return StatusResponse{Workstreams: list, Profiles: profiles, ProviderLimits: state.ProviderLimits, DailyBudget: budget, FailureStreaks: streaks, Diagnostics: diagnostics}
}

// failureStreaks returns the active project's nonzero infrastructure failure
// streaks, ordered by role and profile, or none when no project is active.
func (s *Service) failureStreaks() ([]FailureStreak, error) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() || active == nil {
		return nil, nil
	}
	streams, err := active.repository.Workstreams()
	if err != nil {
		return nil, err
	}
	type attempt struct {
		role string
		trace.TurnAttempt
	}
	var attempts []attempt
	for _, stream := range streams {
		threads, err := active.repository.Threads(stream)
		if err != nil {
			return nil, err
		}
		for _, th := range threads {
			for _, q := range th.Turns {
				for _, a := range q.Attempts {
					// An attempt still running, or one a stop or restart
					// cancelled, says nothing about the plumbing.
					if a.Result != nil && !a.Result.Cancelled {
						attempts = append(attempts, attempt{th.Identity.Role, a})
					}
				}
			}
		}
	}
	slices.SortStableFunc(attempts, func(a, b attempt) int { return a.At.Compare(b.At) })
	streaks := map[[2]string]FailureStreak{}
	for _, a := range attempts {
		key := [2]string{a.role, a.Profile.Name}
		if a.FailureClass != coreadapter.Infrastructure {
			delete(streaks, key)
			continue
		}
		streak := streaks[key]
		streaks[key] = FailureStreak{Role: a.role, Profile: a.Profile.Name, Consecutive: streak.Consecutive + 1, LastFailure: a.Failure, LastAt: a.At}
	}
	var out []FailureStreak
	for _, key := range slices.SortedFunc(maps.Keys(streaks), func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) }) {
		out = append(out, streaks[key])
	}
	return out, nil
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
