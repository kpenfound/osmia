package intake

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// creatingProjectState is the start's state while its new project is created.
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
			if refusal != nil {
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

// ProjectCreationEntries lists the owner's inbox entry for a creation that
// cannot finish, whether it came from a design start, the CLI or the API. It
// offers one decision, retry, which reuses only the repository this creation
// made and never adopts or deletes a repository. A creation that a design
// start began also names that start's workstream and host project.
func (s *Controller) ProjectCreationEntries(projects []*Project) ([]InboxEntry, error) {
	if s.ProjectCreation.Stuck == nil {
		return nil, nil
	}
	state, at, stuck, err := s.ProjectCreation.Stuck()
	if err != nil || !stuck {
		return nil, err
	}
	var stream config.WorkstreamID
	var host config.ProjectID
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
			if found && intent.NewProject != nil && intent.Creation == state.ID {
				stream, host = w, p.ID
			}
		}
	}
	e := Decision(InboxProjectCreation, stream, at, "/projects/create", map[string]any{"id": state.ID})
	e.Project = host
	e.Question = fmt.Sprintf("Creating the project for %s stopped at step %s: %s", state.Repository, state.Step, creationAdvice(state.Stuck))
	e.Blocked = "Creating and registering the project."
	if stream != "" {
		e.Blocked = "Starting the workgroup: the new project is allocated once it is created."
	}
	e.Options = []string{"retry"}
	return []InboxEntry{e}, nil
}

// creationAdvice says what a stuck creation needs. Ownership that could not
// be established is the owner's to resolve on GitHub; a clone failure names the
// host git credential, which the reason carries.
func creationAdvice(stuck *api.ProjectCreationStuck) string {
	if stuck.Kind == api.StuckOwnership || stuck.Kind == api.StuckMismatch {
		return "The repo's ownership could not be established. Resolve the other repo on GitHub (rename or remove it) before retrying; Osmia never adopts or deletes a repository. " + stuck.Reason
	}
	return stuck.Reason + " Retry once the cause is fixed; the retry reuses only the repository this creation made."
}
