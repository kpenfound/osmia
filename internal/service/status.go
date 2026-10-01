package service

import (
	"context"
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

// statuses reports every workstream of every active project, ordered by last
// activity time, newest first, or none when no project or trace is active.
// Last activity is derived only from durable trace records, so a read-only
// request such as this one never changes it. Workstreams that share a last
// activity time are ordered by creation time, newest first, then by
// workstream ID, so the order stays the same across repeated loads and a
// service restart. Archived workstreams are reported and marked archived. The
// librarian's workstream carries no feature and is left out. A
// workstream whose agent turns, unit states, overlap advisories or drift
// rebases cannot be read is reported without them and its first such failure
// in unreadable, by workstream.
func (s *Service) statuses() ([]WorkstreamStatus, map[config.WorkstreamID]Diagnostic, *APIError) {
	cfg, projects := s.runtimes()
	state, _ := s.effective()
	var out []workstreamActivity
	unreadable := map[config.WorkstreamID]Diagnostic{}
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		list, api := s.projectStatuses(active, unreadable)
		if api != nil {
			return nil, nil, api
		}
		for i := range list {
			list[i].status.Archived = archivedIn(state, list[i].status.Workstream)
		}
		out = append(out, list...)
	}
	slices.SortFunc(out, func(a, b workstreamActivity) int {
		if c := b.lastActivity.Compare(a.lastActivity); c != 0 {
			return c
		}
		if c := b.createdAt.Compare(a.createdAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.status.Workstream), string(b.status.Workstream))
	})
	statuses := []WorkstreamStatus{}
	for _, w := range out {
		statuses = append(statuses, w.status)
	}
	return statuses, unreadable, nil
}

// workstreamActivity pairs a reported status with the trace facts that order
// the workstream list: last activity time, newest first, then creation time,
// newest first, then workstream ID.
type workstreamActivity struct {
	status       WorkstreamStatus
	lastActivity time.Time
	createdAt    time.Time
}

// projectStatuses reports every workstream of one project, as statuses does,
// unordered.
func (s *Service) projectStatuses(active *activeProject, unreadable map[config.WorkstreamID]Diagnostic) ([]workstreamActivity, *APIError) {
	out := []workstreamActivity{}
	list, err := active.repository.Statuses()
	if err != nil {
		return nil, &APIError{Internal, fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", active.id)}
	}
	mode := s.Context().Mode(active.id)
	librarian := librarianWorkstream(active.id)
	for _, w := range list {
		if w.Workstream == librarian {
			continue
		}
		view := statusView(active.id, mode, w)
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
		out = append(out, workstreamActivity{status: view, lastActivity: w.LastActivity, createdAt: w.CreatedAt})
	}
	return out, nil
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
	diagnostics = append(diagnostics, s.notifier.diagnostics()...)
	usage, unread := s.providerUsage(state, profiles)
	if unread != nil {
		diagnostics = append(diagnostics, *unread)
	}
	// A jj that hangs must not hold up status.
	checking, cancel := context.WithTimeout(context.Background(), jjCheckTimeout)
	workspaces, err := s.newWorkspaces(checking, s.current())
	cancel()
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{"workspaces", Unavailable, workspaces.Problem})
	}
	list, unreadable, api := s.statuses()
	if api != nil {
		list = []WorkstreamStatus{}
	}
	capacity, unread := s.capacityStatus(list)
	if unread != nil {
		diagnostics = append(diagnostics, *unread)
	}
	if api != nil {
		return StatusResponse{Workstreams: list, Profiles: profiles, ProviderLimits: state.ProviderLimits, DailyBudget: budget, Capacity: capacity, ProviderUsage: usage, FailureStreaks: streaks, Diagnostics: append(diagnostics, Diagnostic{"workstreams", api.Code, api.Message}), Workspaces: workspaces}
	}
	for _, w := range list {
		if d, ok := unreadable[w.Workstream]; ok {
			diagnostics = append(diagnostics, d)
		}
	}
	return StatusResponse{Workstreams: list, Profiles: profiles, ProviderLimits: state.ProviderLimits, DailyBudget: budget, Capacity: capacity, ProviderUsage: usage, FailureStreaks: streaks, Diagnostics: diagnostics, Workspaces: workspaces}
}

// failureStreaks returns the nonzero infrastructure failure streaks across
// every active project, ordered by role and profile, or none when no project
// is active.
func (s *Service) failureStreaks() ([]FailureStreak, error) {
	cfg, projects := s.runtimes()
	type attempt struct {
		role string
		trace.TurnAttempt
	}
	var attempts []attempt
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		streams, err := active.repository.Workstreams()
		if err != nil {
			return nil, err
		}
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
	if len(s.current().Projects) == 0 {
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
	return WorkstreamStatus{}, &APIError{NotFound, fmt.Sprintf("workstream %s is not in an active project; list workstreams with osmia status", id)}
}

func statusView(project config.ProjectID, mode bundle.Mode, w trace.WorkstreamStatus) WorkstreamStatus {
	out := WorkstreamStatus{Workstream: w.Workstream, Project: project, Workspaces: w.Workspaces, Units: []UnitStatus{}, Advisories: []OverlapAdvisory{}, OpenQuestions: w.OpenQuestions, Gates: append([]trace.OwnerGate{}, w.Gates...), ContextMode: mode}
	if w.State != "" {
		state := w.State
		out.State = &state
	}
	if st := w.Status; st != nil {
		out.Status = &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}
	}
	return out
}
