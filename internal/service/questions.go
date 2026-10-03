package service

import (
	"context"
	"fmt"

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
	files := s.contextFor(repository)
	return func(ctx context.Context, stream config.WorkstreamID) (string, error) {
		context, err := chiefContext(ctx, files, repository, project, stream)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(chiefEvents, stream) + "\n\n" + chiefDocumentsGuidance + "\n\n" + questions.Guidance + "\n\n" + contestGuidance + "\n\n" + context, nil
	}
}

// answers returns the pass that queues recorded answers on their askers'
// threads, with the profile each asker would start a new turn with when the
// answer is delivered. Abandoned workstreams keep their answers undelivered.
func (s *Service) answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return &questions.Deliverer{Repository: repository, Now: s.now,
		Profile: func(role, agent string) (coreadapter.Profile, error) {
			return s.agentProfile(cfg, role, agent)
		},
		Skip: func(stream config.WorkstreamID) (bool, error) { return abandoned(repository, stream) }}
}
