package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefEvents opens the system prompt of every event turn; the question
// guidance and the workstream's rendered bundle follow it.
const chiefEvents = "You are the chief of staff for workstream %s. The prompt lists service events. The project context below was assembled when the events were delivered."

// chiefEventsOtherMember notes, for a workgroup of more than one member,
// that this delivery's events are about a member other than the chief's own
// host workstream, and that the chief must qualify that member's questions.
const chiefEventsOtherMember = " These events are about workstream %s, a member of your workgroup; answer or escalate one of its questions as \"%s:<number>\"."

func (s *Service) chiefEventsSkip(repository *trace.Repository) func(config.WorkstreamID) (bool, error) {
	skip := chiefOfStaffSkip(repository)
	return func(stream config.WorkstreamID) (bool, error) {
		if skipped, err := skip(stream); err != nil || skipped {
			return skipped, err
		}
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil || id == "" {
			return false, err
		}
		_, g, _, err := repository.CrossRepoProjectSpec(id)
		if err != nil {
			return false, err
		}
		if g.ID != id || len(g.Members) < 2 {
			return false, nil
		}
		return s.workgroupFullyRetired(context.Background(), g)
	}
}

// chiefEventsPrompt returns the system prompt of a workstream's event turns.
// The bundle is assembled from the project's files and the given trace only.
// stream is the member whose events this delivery carries; the chief of
// staff is identified by its workgroup's host, which may be a different
// workstream when stream shares a chief of staff with others.
func (s *Service) chiefEventsPrompt(project config.ProjectID, repository *trace.Repository) func(context.Context, config.WorkstreamID) (string, error) {
	return func(ctx context.Context, stream config.WorkstreamID) (string, error) {
		// repository is the caller's own long-lived instance (events.New's
		// pass), so chiefRepository's per-call resolution must never close
		// it out from under the rest of the pass. For an ordinary
		// workstream or a same-repo workgroup member it returns repository
		// itself with a no-op close; for a cross-repo workgroup's member it
		// opens that workgroup's own trace transiently for this call alone,
		// and closeTarget is what actually releases it.
		target, host, bundleStream, closeTarget, err := s.chiefRepository(ctx, repository, stream)
		if errors.Is(err, errWorkgroupJoining) {
			return "", fmt.Errorf("%w: %v", events.ErrChiefUnavailable, err)
		}
		if err != nil {
			return "", err
		}
		bundleRepo, err := target.ProjectFor(bundleStream)
		if err != nil {
			closeTarget()
			return "", err
		}
		context, err := chiefContext(ctx, s.contextFor(bundleRepo), target, closeTarget, bundleRepo.Project(), bundleStream)
		if err != nil {
			return "", err
		}
		note := ""
		if host != stream {
			note = fmt.Sprintf(chiefEventsOtherMember, stream, stream)
		}
		return fmt.Sprintf(chiefEvents, host) + note + "\n\n" + chiefDocumentsGuidance + "\n\n" + questions.Guidance + "\n\n" + amendmentGuidance + "\n\n" + contestGuidance + "\n\n" + driftHandbackGuidance + "\n\n" + ownerWritingGuidance + "\n\n" + context, nil
	}
}

// answers returns the pass that queues recorded answers on their askers'
// threads, with the profile each asker would start a new turn with when the
// answer is delivered. Abandoned workstreams keep their answers undelivered,
// and so does a question whose unit merged before its answer arrived: the
// unit's workspace is gone and a merged unit changes only through a
// follow-up.
func (s *Service) answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return &questions.Deliverer{Repository: repository, Now: s.now,
		Profile: func(role, agent string) (coreadapter.Profile, error) {
			return s.agentProfile(cfg, role, agent)
		},
		Skip: func(stream config.WorkstreamID) (bool, error) { return abandoned(repository, stream) },
		Settled: func(q trace.Question) (bool, error) {
			if q.Unit == "" {
				return false, nil
			}
			state, err := repository.Workflow(q.Workstream, trace.UnitSubject(q.Unit))
			return state.Value == UnitMerged, err
		}}
}
