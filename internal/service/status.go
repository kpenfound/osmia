package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/trace"
)

type statusesRead struct {
	list       []WorkstreamStatus
	unreadable map[config.WorkstreamID]Diagnostic
	api        *APIError
}

func (s *Service) statuses() ([]WorkstreamStatus, map[config.WorkstreamID]Diagnostic, *APIError) {
	snapshot, err := readview.Consistent(func() (statusesRead, error) {
		list, unreadable, api := s.statusesSnapshot()
		return statusesRead{list, unreadable, api}, nil
	}, s.groupApprovalGeneration)
	if err != nil {
		return nil, nil, &APIError{Code: Internal, Message: err.Error()}
	}
	return snapshot.list, snapshot.unreadable, snapshot.api
}

// groupApprovalGeneration covers group authority independently of member
// generations. A completion racing a multi-repo status read retries the whole
// view, so one response cannot combine pending and approved group members.
func (s *Service) groupApprovalGeneration() uint64 {
	_, projects := s.runtimes()
	decisions := map[config.WorkgroupID]string{}
	for _, p := range projects {
		streams, err := p.repository.Workstreams()
		if err != nil {
			return 0
		}
		for _, stream := range streams {
			id, err := p.repository.CrossRepoWorkgroupOf(stream)
			if err != nil {
				return 0
			}
			if id == "" {
				continue
			}
			if _, seen := decisions[id]; seen {
				continue
			}
			approval, found, err := p.repository.GroupApprovalOf(stream)
			if err != nil {
				return 0
			}
			decisions[id] = "pending"
			if found {
				data, _ := json.Marshal(approval)
				decisions[id] = string(data)
			}
		}
	}
	digest := sha256.New()
	for _, id := range slices.Sorted(maps.Keys(decisions)) {
		digest.Write([]byte(string(id) + ":" + decisions[id] + "\n"))
	}
	return binary.LittleEndian.Uint64(digest.Sum(nil))
}

func (s *Service) statusesSnapshot() ([]WorkstreamStatus, map[config.WorkstreamID]Diagnostic, *APIError) {
	cfg, projects := s.runtimes()
	var out []workstreamActivity
	unreadable := map[config.WorkstreamID]Diagnostic{}
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		// state is read inside the retried attempt, alongside the trace, so
		// a workstream's archived flag and the runtime-derived rows below
		// never come from a different runtime generation than the rest of
		// this project's trace-read rows: a racing archive, or any other
		// runtime mutation, forces the whole attempt to retry rather than
		// land in a response assembled from two different moments.
		found, err := readview.Consistent(func() (projectStatusResult, error) {
			state, _ := s.effective()
			list, projectUnreadable, api := s.projectStatuses(active)
			if api != nil {
				return projectStatusResult{}, apiErrAsError{Api: api}
			}
			var rows []workstreamActivity
			for i := range list {
				list[i].status.Archived = archivedIn(state, list[i].status.Workstream)
			}
			for _, w := range list {
				if !w.status.Archived {
					rows = append(rows, w)
				}
			}
			for _, a := range state.Archived {
				if a.Project != active.id {
					continue
				}
				title, terminal := a.Title, a.State
				if title == "" {
					for _, w := range list {
						if w.status.Workstream == a.Workstream {
							if w.status.Status != nil {
								title = w.status.Status.Goal
							}
							if w.status.State != nil {
								terminal = *w.status.State
							}
						}
					}
				}
				if title == "" {
					title = string(a.Workstream)
				}
				archivedView := WorkstreamStatus{Project: a.Project, Workstream: a.Workstream, Archived: true, TraceDeleted: !a.CleanedAt.IsZero(), DeliveryLink: a.DeliveryLink, State: &terminal, Status: &StatusView{Goal: title}, Units: []UnitStatus{}, Advisories: []OverlapAdvisory{}}
				if err := presentArchivedWorkstream(active.repository, &archivedView, a); err != nil {
					projectUnreadable[a.Workstream] = Diagnostic{Field: "presentation", Code: Internal, Message: err.Error()}
				}
				rows = append(rows, workstreamActivity{status: archivedView, lastActivity: a.ArchivedAt, createdAt: a.ArchivedAt})
			}
			return projectStatusResult{rows, projectUnreadable}, nil
		}, active.repository.Generation, s.storeGeneration)
		if err != nil {
			var wrapped apiErrAsError
			if errors.As(err, &wrapped) {
				return nil, nil, wrapped.Api
			}
			return nil, nil, &APIError{Code: Internal, Message: err.Error()}
		}
		out = append(out, found.list...)
		for id, d := range found.unreadable {
			unreadable[id] = d
		}
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

// projectStatusResult is one consistent attempt of projectStatuses: the
// workstream activity it found and the diagnostic of the first thing it
// could not read, by workstream.
type projectStatusResult struct {
	list       []workstreamActivity
	unreadable map[config.WorkstreamID]Diagnostic
}

// projectStatuses reports every workstream of one project, as statuses does,
// unordered. Its own unreadable map holds only the diagnostics of this one
// attempt, so a caller that retries the whole attempt through consistentRead
// never carries a diagnostic from an earlier, discarded attempt into a later
// one that read the workstream cleanly.
func (s *Service) projectStatuses(active *activeProject) ([]workstreamActivity, map[config.WorkstreamID]Diagnostic, *APIError) {
	out := []workstreamActivity{}
	unreadable := map[config.WorkstreamID]Diagnostic{}
	list, err := active.repository.Statuses()
	if err != nil {
		return nil, nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", active.id)}
	}
	mode := s.Context().Mode(active.id)
	librarian := librarianWorkstream(active.id)
	for _, w := range list {
		if w.Workstream == librarian {
			continue
		}
		design, err := isDesignWorkstream(active.repository, w.Workstream)
		if err != nil {
			return nil, nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", w.Workstream)}
		}
		targetStream, err := s.designStatusAddress(active.repository, w.Workstream)
		if err != nil {
			return nil, nil, &APIError{Code: Internal, Message: err.Error()}
		}
		if targetStream != w.Workstream {
			continue
		}
		view := statusView(active.id, mode, w, design)
		approval, accepted, err := active.repository.GroupApprovalOf(w.Workstream)
		if err != nil {
			return nil, nil, &APIError{Code: Internal, Message: err.Error()}
		}
		if accepted {
			view.Gates = slices.DeleteFunc(view.Gates, func(g trace.OwnerGate) bool { return g.Kind == "ratification" })
		}
		group, err := active.repository.CrossRepoWorkgroupOf(w.Workstream)
		if err != nil {
			return nil, nil, &APIError{Code: Internal, Message: err.Error()}
		}
		if group != "" {
			chief, host, _, close, err := s.chiefRepository(context.Background(), active.repository, w.Workstream)
			if err == nil {
				statuses, readErr := trace.Read[trace.Status](chief, host)
				close()
				if readErr != nil {
					return nil, nil, &APIError{Code: Internal, Message: readErr.Error()}
				}
				if len(statuses) > 0 {
					st := statuses[len(statuses)-1]
					view.Status = &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}
				}
			} else if !errors.Is(err, errWorkgroupJoining) {
				return nil, nil, &APIError{Code: Internal, Message: err.Error()}
			}
		}

		agents, err := agentStatuses(active.repository, w.Workstream, time.Now())
		if err == nil {
			err = s.step("status-agents")
		}
		if err != nil {
			unreadable[w.Workstream] = Diagnostic{Field: "agents", Code: Internal, Message: fmt.Sprintf("cannot read the agent turns of workstream %s; check the trace repository", w.Workstream)}
		} else {
			view.Agents = agents
		}
		units, err := unitStates(active.repository, w.Workstream, w.Subjects)
		if err == nil {
			err = s.step("status-units")
		}
		if err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{Field: "units", Code: Internal, Message: fmt.Sprintf("cannot read the unit states of workstream %s; check the trace repository", w.Workstream)}
			}
			units = nil
		}
		view.Units = append(view.Units, units...)
		advisories, err := overlapAdvisories(active.repository, w.Workstream, w.Subjects)
		if err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{Field: "advisories", Code: Internal, Message: fmt.Sprintf("cannot read the overlap advisories of workstream %s; check the trace repository", w.Workstream)}
			}
		} else {
			view.Advisories = advisories
		}
		drift, err := latestDrift(active.repository, w.Workstream, w.Subjects)
		if err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{Field: "drift", Code: Internal, Message: fmt.Sprintf("cannot read the drift rebases of workstream %s; check the trace repository", w.Workstream)}
			}
		} else {
			view.Drift = drift
		}
		if wait, err := edgeWaitDisplay(active.repository, w.Workstream); err != nil {
			if _, ok := unreadable[w.Workstream]; !ok {
				unreadable[w.Workstream] = Diagnostic{Field: "edges", Code: Internal, Message: fmt.Sprintf("cannot read the workgroup edges of workstream %s; check the trace repository", w.Workstream)}
			}
		} else {
			view.Wait = wait
		}
		for i := range view.Units {
			for _, gate := range view.Gates {
				if gate.Kind == UnitContested && gate.Reference == view.Units[i].Unit && gate.Reason != "" {
					view.Units[i].Reason = gate.Reason
				}
			}
		}
		if accepted && !approval.Complete {
			_, detail, err := s.groupApprovalDetail(approval)
			if err != nil {
				return nil, nil, &APIError{Code: Internal, Message: err.Error()}
			}
			view.Wait = detail
		}
		if err := s.presentWorkstream(active.repository, &view); err != nil {
			unreadable[w.Workstream] = Diagnostic{Field: "presentation", Code: Internal, Message: err.Error()}
		}
		out = append(out, workstreamActivity{status: view, lastActivity: w.LastActivity, createdAt: w.CreatedAt})
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
	budget := s.dailyBudgetStatus()
	diagnostics := []Diagnostic{}
	streaks, err := s.failureStreaks()
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{Field: "failure_streaks", Code: Internal, Message: "cannot read the turn attempts of the active project; check the trace repository"})
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
		diagnostics = append(diagnostics, Diagnostic{Field: "workspaces", Code: Unavailable, Message: workspaces.Problem})
	}
	list, unreadable, api := s.statuses()
	if api != nil {
		list = []WorkstreamStatus{}
	}
	capacity := s.capacityStatus(list)
	if api != nil {
		return StatusResponse{Workstreams: list, Profiles: profiles, ProviderLimits: state.ProviderLimits, DailyBudget: budget, Capacity: capacity, ProviderUsage: usage, FailureStreaks: streaks, Diagnostics: append(diagnostics, Diagnostic{Field: "workstreams", Code: api.Code, Message: api.Message}), Workspaces: workspaces}
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
		return WorkstreamStatus{}, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	if len(s.current().Projects) == 0 {
		return WorkstreamStatus{}, &APIError{Code: NoProject, Message: "no project is configured; add one with osmia project add"}
	}
	list, unreadable, api := s.statuses()
	if api != nil {
		return WorkstreamStatus{}, api
	}
	// Archive records and ordinary rows need no live conversation history.
	// Only an address omitted from the list can be a continued design alias.
	if !slices.ContainsFunc(list, func(w WorkstreamStatus) bool { return w.Workstream == id }) {
		cfg, projects := s.runtimes()
		for _, active := range projects {
			if !cfg.Active(active.id) || id == librarianWorkstream(active.id) {
				continue
			}
			streams, err := active.repository.Workstreams()
			if err != nil {
				return WorkstreamStatus{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.id)}
			}
			if slices.Contains(streams, id) {
				id, err = s.designStatusAddress(active.repository, id)
				if err != nil {
					return WorkstreamStatus{}, &APIError{Code: Internal, Message: err.Error()}
				}
				break
			}
		}
	}
	if d, ok := unreadable[id]; ok {
		return WorkstreamStatus{}, &APIError{Code: d.Code, Message: d.Message}
	}
	for _, w := range list {
		if w.Workstream == id {
			return w, nil
		}
	}
	return WorkstreamStatus{}, &APIError{Code: NotFound, Message: fmt.Sprintf("workstream %s is not in an active project; list workstreams with osmia status", id)}
}

// designStatusAddress maps an adopted design host to its first live member,
// or its historical first member once none remain live.
// Status resolution reads membership without requiring live conversation history.
func (s *Service) designStatusAddress(repo *trace.Repository, stream config.WorkstreamID) (config.WorkstreamID, error) {
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil || id == "" {
		return stream, err
	}
	_, g, _, err := repo.CrossRepoProjectSpec(id)
	if err != nil {
		return stream, err
	}
	if g.Conversation != nil && g.Conversation.Workstream == stream && len(g.Members) > 1 {
		leader, err := s.firstLiveWorkgroupMember(g)
		if err != nil {
			return "", err
		}
		if leader != "" {
			return leader, nil
		}
		return g.Members[1], nil
	}
	return stream, nil
}

func statusView(project config.ProjectID, mode bundle.Mode, w trace.WorkstreamStatus, design bool) WorkstreamStatus {
	kind := w.Kind
	if kind == "" && design {
		// design is isDesignWorkstream's own answer, read directly from the
		// design-session marker openDesign records, never inferred here from
		// an empty kind and an empty feature state: an ordinary workstream
		// can legitimately have both and still not be a design session. The
		// web page and CLI use this marker to show the design chat instead
		// of the ordinary workstream view.
		kind = "design"
	}
	out := WorkstreamStatus{Workstream: w.Workstream, Project: project, Workspaces: w.Workspaces, Kind: kind, Profile: w.Profile, Base: w.Base, Branch: w.Branch, Title: w.Title,
		Units: []UnitStatus{}, Advisories: []OverlapAdvisory{}, OpenQuestions: w.OpenQuestions, Gates: append([]trace.OwnerGate{}, w.Gates...), ContextMode: mode}
	if w.State != "" {
		state := w.State
		out.State = &state
	}
	if st := w.Status; st != nil {
		out.Status = &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}
	}
	return out
}
