package service

import (
	"context"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefEvents opens the system prompt of every event turn; the question
// guidance and the workstream's rendered bundle follow it.
const chiefEvents = "You are the chief of staff for workstream %s. The prompt lists service events. The project context below was assembled when the events were delivered."

// chiefEventsPrompt returns the system prompt of a workstream's event turns.
// The bundle is assembled from the project's files and the given trace only.
func (s *Service) chiefEventsPrompt(project config.ProjectID, repository *trace.Repository) func(context.Context, config.WorkstreamID) (string, error) {
	files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repository, nil }, Now: func() time.Time { return time.Now().UTC() }}
	return func(ctx context.Context, stream config.WorkstreamID) (string, error) {
		b, err := files.Assemble(ctx, project, bundle.Scope{Workstream: stream})
		if err != nil {
			return "", fmt.Errorf("context of workstream %s: %w", stream, err)
		}
		return fmt.Sprintf(chiefEvents, stream) + "\n\n" + questions.Guidance + "\n\n" + b.Render(), nil
	}
}

// answers returns the pass that queues recorded answers on their askers'
// threads, with the profile each asker's role is bound to when the answer is
// delivered. Abandoned workstreams keep their answers undelivered.
func (s *Service) answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return &questions.Deliverer{Repository: repository, Now: s.now,
		Profile: func(role string) (coreadapter.Profile, error) {
			profile, _, err := s.roleExecution(cfg, role)
			return profile, err
		},
		Skip: func(stream config.WorkstreamID) (bool, error) { return abandoned(repository, stream) }}
}
