package intake

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

// ChiefEvents opens the system prompt of every event turn; the question
// guidance and the workstream's rendered bundle follow it.
const ChiefEvents = "You are the chief of staff for workstream %s. The prompt lists service events. The project context below was assembled when the events were delivered."

// ChiefEventsOtherMember notes, for a workgroup of more than one member,
// that this delivery's events are about a member other than the chief's own
// host workstream, and that the chief must qualify that member's questions.
const ChiefEventsOtherMember = " These events are about workstream %s, a member of your workgroup; answer or escalate one of its questions as \"%s:<number>\"."

// FollowUpGuidance tells the chief of staff how to keep work moving: read the
// work each action queued, and check back with follow_up instead of promising
// the owner that work will happen.
const FollowUpGuidance = "Events and messages are information you act on to keep the work moving. Every action tool's result lists under queued the work the service queued or is running because of it, or says under nothing_will_run why nothing will run and until when. Tell the owner work is under way only when a result shows it queued. When nothing is queued and you expect something to change later, call follow_up with after (5m to 24h), a reason and, for a shared chief, the workstream, and you get a turn back then with what changed; a newer follow_up for the same workstream replaces an unfired one, and your pending follow-ups are listed in the project context. When an action of yours starts no work and you schedule no follow_up, the service schedules one at the stall window."

func (s *Controller) ChiefEventsSkip(repository *trace.Repository) func(config.WorkstreamID) (bool, error) {
	skip := ChiefOfStaffSkip(repository)
	return func(stream config.WorkstreamID) (bool, error) {
		if skipped, err := skip(stream); err != nil || skipped {
			return skipped, err
		}
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil || id == "" {
			return false, err
		}
		g, _, err := repository.CrossRepoWorkgroup(id)
		if err != nil {
			return false, err
		}
		if g.ID != id || len(g.Members) < 2 {
			return false, nil
		}
		return s.WorkgroupFullyRetired(context.Background(), g)
	}
}

// ChiefEventsPrompt returns the system prompt of a workstream's event turns.
// The bundle is assembled from the project's files and the given trace only.
// stream is the member whose events this delivery carries; the chief of
// staff is identified by its workgroup's host, which may be a different
// workstream when stream shares a chief of staff with others.
func (s *Controller) ChiefEventsPrompt(project config.ProjectID, repository *trace.Repository) func(context.Context, config.WorkstreamID) (string, error) {
	return func(ctx context.Context, stream config.WorkstreamID) (string, error) {
		target, host, bundleStream, closeTarget, err := s.ChiefRepository(ctx, repository, stream)
		if errors.Is(err, ErrWorkgroupJoining) {
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
		context, err := s.ChiefContext(ctx, s.ContextFor(bundleRepo), target, closeTarget, bundleRepo.Project(), bundleStream)
		if err != nil {
			return "", err
		}
		note := ""
		if host != stream {
			note = fmt.Sprintf(ChiefEventsOtherMember, stream, stream)
		}
		return fmt.Sprintf(ChiefEvents, host) + note + "\n\n" + FollowUpGuidance + "\n\n" + ChiefDocumentsGuidance + "\n\n" + questions.Guidance + "\n\n" + AmendmentGuidance + "\n\n" + ContestGuidance + "\n\n" + ObjectionGuidance + "\n\n" + DriftHandbackGuidance + "\n\n" + OwnerWritingGuidance + "\n\n" + context, nil
	}
}

// Answers returns the pass that queues recorded answers on their askers'
// threads, with the profile each asker would start a new turn with when the
// answer is delivered. Abandoned workstreams keep their answers undelivered,
// and so does a question whose unit merged before its answer arrived: the
// unit's workspace is gone and a merged unit changes only through a
// follow-up.
func (s *Controller) Answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return &questions.Deliverer{Repository: repository, Now: s.Now, Workstreams: func() ([]config.WorkstreamID, error) { return NonSessionWorkstreams(repository) },
		Profile: func(role, agent string) (coreadapter.Profile, error) {
			return s.AgentProfile(cfg, role, agent)
		},
		Skip: func(stream config.WorkstreamID) (bool, error) { return Abandoned(repository, stream) },
		Settled: func(q trace.Question) (bool, error) {
			if q.Unit == "" {
				return false, nil
			}
			state, err := repository.Workflow(q.Workstream, trace.UnitSubject(q.Unit))
			return state.Value == UnitMerged, err
		}}
}
