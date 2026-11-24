package intake

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const creatingProjectState = "creating-project"

// CheckDesignNewProject refuses, before anything is recorded or created, a
// start whose new project could not be created: creation is off, the token is
// missing or is for another account, the repo name is taken or the clone path
// exists.
func (s *Controller) CheckDesignNewProject(ctx context.Context, project *api.NewProject) *APIError {
	if s.ProjectCreation.Check == nil {
		return &APIError{Code: Unavailable, Message: "project creation is not available"}
	}
	return s.ProjectCreation.Check(ctx, newProjectRequest("", project))
}

func newProjectRequest(id config.ProjectID, project *api.NewProject) api.ProjectCreateRequest {
	return api.ProjectCreateRequest{ID: id, Name: project.Name, Repo: project.Repo, Description: project.Description,
		Visibility: project.Visibility, Charter: project.Charter}
}

// EnsureDesignProject looks the start's creation identity up and reports
// whether the new project is registered and active. A complete creation is
// ready. A pending one is resumed under the same identity unless its last
// attempt stopped for the owner, which waits for the owner's retry from the
// inbox. One that never started runs under the same identity. A creation that
// fails is logged and tried again on the next pass or retried by the owner; it
// is not a failure of the pass, which would hold back the project's other
// scheduling.
func (s *Controller) EnsureDesignProject(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, intent DesignStartIntent) (bool, error) {
	creation := s.ProjectCreation
	lookup := func() (api.ProjectCreationState, bool) {
		state, refusal := creation.State(intent.Creation)
		if refusal != nil {
			s.Log().Error("cannot read the creation of a design start's new project", "workstream", string(stream), "creation", string(intent.Creation), "error", refusal.Message)
			return state, false
		}
		return state, true
	}
	state, ok := lookup()
	if !ok {
		return false, nil
	}
	if state.State != api.CreationComplete {
		if err := s.DesignStartState(ctx, r, stream, creatingProjectState); err != nil {
			return false, err
		}
		var request api.ProjectCreateRequest
		switch state.State {
		case api.CreationPending:
			if state.Stuck != nil {
				return false, nil
			}
			request = api.ProjectCreateRequest{ID: intent.Creation}
		default:
			request = newProjectRequest(intent.Creation, intent.NewProject)
		}
		_, refusal := creation.Create(ctx, request)
		if state, ok = lookup(); !ok {
			return false, nil
		}
		if state.State != api.CreationComplete {
			if refusal != nil && state.State == api.CreationNotStarted {
				if err := s.RecordDesignCreationRefusal(ctx, r, stream, intent, refusal.Message); err != nil {
					return false, err
				}
			} else if refusal != nil {
				s.Log().Error("a design start's new project is not created", "workstream", string(stream), "creation", string(intent.Creation), "error", refusal.Message)
			}
			return false, ctx.Err()
		}
	}
	if err := s.Step("design-project-created"); err != nil {
		return false, err
	}
	return true, nil
}

// DesignCreationRefusalSubject is the workflow subject of a design start
// whose new project's creation was refused before it began: refused-<n> once
// the n-th distinct refusal is recorded.
const DesignCreationRefusalSubject = "design-project-refusal"

// DesignCreationRefusal returns the latest recorded refusal of the design
// start's new project, and whether one is recorded.
func DesignCreationRefusal(r *trace.Repository, stream config.WorkstreamID) (trace.Transition, bool, error) {
	transitions, err := trace.Read[trace.Transition](r, stream)
	if err != nil {
		return trace.Transition{}, false, err
	}
	for i := len(transitions) - 1; i >= 0; i-- {
		if transitions[i].Subject == DesignCreationRefusalSubject {
			return transitions[i], true, nil
		}
	}
	return trace.Transition{}, false, nil
}

// RecordDesignCreationRefusal records why the design start's new project
// could not be created, once per distinct reason, and tells the chief of
// staff of the design's conversation with a turn of its own. The start keeps
// trying the creation on every pass, since the cause, a missing token or a
// repo name or clone path taken since the start, may be put right.
func (s *Controller) RecordDesignCreationRefusal(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, intent DesignStartIntent, reason string) error {
	latest, found, err := DesignCreationRefusal(r, stream)
	if err != nil {
		return err
	}
	if !found || latest.Reason != reason {
		state, err := r.Workflow(stream, DesignCreationRefusalSubject)
		if err != nil {
			return err
		}
		n := state.Version + 1
		id := fmt.Sprintf("design-project-refused-%d", n)
		s.Log().Error("a design start's new project is refused", "workstream", string(stream), "creation", string(intent.Creation), "error", reason)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.Project(), Workstream: stream, At: s.Now(), Actor: ServiceActor, Cause: DesignStartTransition}
		latest = trace.Transition{Header: h, Subject: DesignCreationRefusalSubject, From: state.Value, To: fmt.Sprintf("refused-%d", n), Reason: reason}
		if _, err := r.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: latest}); errors.Is(err, trace.ErrConflict) {
			return nil
		} else if err != nil {
			return err
		}
	}
	return s.NotifyDesignCreationRefusal(ctx, r, stream, intent, latest)
}

// NotifyDesignCreationRefusal enqueues the chief of staff's turn that tells
// it of the recorded refusal, once.
func (s *Controller) NotifyDesignCreationRefusal(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, intent DesignStartIntent, refusal trace.Transition) error {
	th, err := r.EnsureChiefOfStaff(ctx, stream, s.Now(), ServiceActor)
	if err != nil {
		return err
	}
	if QueuedTurn(th, refusal.ID) {
		return nil
	}
	profile, _, err := s.RoleExecution(s.Current(), trace.ChiefOfStaff)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("The new project %s that this design's start creates could not be created: %s. The workgroup does not start until it is. The service tries the creation again on every pass, so it goes on once the cause is put right (for example GITHUB_TOKEN set in the service's environment, or the repo name or clone path freed); the owner's inbox shows the refusal. Tell the owner in plain words what happened and what to fix.", intent.NewProject.Repo, refusal.Reason)
	host := th.Identity.Workstream
	_, err = r.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + refusal.ID, Revision: 1, Project: r.Project(), Workstream: host, At: s.Now(), Actor: ServiceActor, Cause: refusal.ID},
		AgentID: trace.ChiefOfStaff, ThreadID: th.Identity.ThreadID, TurnID: refusal.ID, Profile: profile, SystemPrompt: "The service reports a refused project creation below. Tell the owner briefly what it means and what to fix.", Prompt: body})
	return err
}

// ProjectCreationEntries lists the owner's inbox entry for a creation that
// cannot finish, whether it came from a design start, the CLI or the API. It
// offers one decision, retry, which reuses only the repository this creation
// made and never adopts or deletes a repository. A creation that a design
// start began also names that start's workstream and host project. A design
// start whose new project was refused before its creation began has an entry
// too, with the refusal; its retry asks for the creation again, which the
// service also does on every pass.
func (s *Controller) ProjectCreationEntries(projects []*Project) ([]InboxEntry, error) {
	if s.ProjectCreation.Stuck == nil {
		return nil, nil
	}
	state, at, stuck, err := s.ProjectCreation.Stuck()
	if err != nil {
		return nil, err
	}
	var stream config.WorkstreamID
	var host config.ProjectID
	var refused []InboxEntry
	for _, p := range projects {
		streams, err := p.Repository.Workstreams()
		if err != nil {
			return nil, err
		}
		for _, w := range streams {
			intent, found, err := DesignStartIntentOf(p.Repository, w)
			if err != nil {
				return nil, err
			}
			if !found || intent.NewProject == nil {
				continue
			}
			if stuck && intent.Creation == state.ID {
				stream, host = w, p.ID
				continue
			}
			e, open, err := s.refusedCreationEntry(p, w, intent)
			if err != nil {
				return nil, err
			}
			if open {
				refused = append(refused, e)
			}
		}
	}
	if !stuck {
		return refused, nil
	}
	e := Decision(InboxProjectCreation, stream, at, "/projects/create", map[string]any{"id": state.ID})
	e.Project = host
	e.Question = fmt.Sprintf("Creating the project for %s stopped at step %s: %s", state.Repository, state.Step, creationAdvice(state.Stuck))
	e.Blocked = "Creating and registering the project."
	if stream != "" {
		e.Blocked = "Starting the workgroup: the new project is allocated once it is created."
	}
	e.Options = []string{"retry"}
	return append(refused, e), nil
}

func (s *Controller) refusedCreationEntry(p *Project, stream config.WorkstreamID, intent DesignStartIntent) (InboxEntry, bool, error) {
	refusal, found, err := DesignCreationRefusal(p.Repository, stream)
	if err != nil || !found || s.ProjectCreation.State == nil {
		return InboxEntry{}, false, err
	}
	state, refusedRead := s.ProjectCreation.State(intent.Creation)
	if refusedRead != nil {
		return InboxEntry{}, false, fmt.Errorf("creation %s: %s", intent.Creation, refusedRead.Message)
	}
	if state.State != api.CreationNotStarted {
		return InboxEntry{}, false, nil
	}
	req := newProjectRequest(intent.Creation, intent.NewProject)
	body := map[string]any{"id": req.ID, "name": req.Name, "repo": req.Repo, "charter": req.Charter}
	if req.Description != "" {
		body["description"] = req.Description
	}
	if req.Visibility != "" {
		body["visibility"] = req.Visibility
	}
	e := Decision(InboxProjectCreation, stream, refusal.At, "/projects/create", body)
	e.Project = p.ID
	e.Question = fmt.Sprintf("Creating the project for %s was refused before it began: %s. The service tries again on every pass, so it goes on once the cause is fixed; retry to try now.", intent.NewProject.Repo, refusal.Reason)
	e.Blocked = "Starting the workgroup: the new project is allocated once it is created."
	e.Options = []string{"retry"}
	return e, true, nil
}

func creationAdvice(stuck *api.ProjectCreationStuck) string {
	if stuck.Kind == api.StuckOwnership || stuck.Kind == api.StuckMismatch {
		return "The repo's ownership could not be established. Resolve the other repo on GitHub (rename or remove it) before retrying; Osmia never adopts or deletes a repository. " + stuck.Reason
	}
	return stuck.Reason + " Retry once the cause is fixed; the retry reuses only the repository this creation made."
}
