package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

var errAbandonmentRouting = errors.New("the abandonment is being routed")

func (s *Controller) abandonForChange(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, c WorkgroupChange) error {
	if s.Router == nil {
		return errors.New("routing the abandonment of a workstream is unavailable")
	}
	st, err := repo.RawWorkflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if st.Value == AbandonedState {
		return nil
	}
	id, cause := "cancel-"+c.ID, WorkgroupChangeDocument(c.ID)
	body := fmt.Sprintf("Workstream state changed to %s: %s", AbandonedState, c.Request.Change)
	if st.Value != "" {
		body = fmt.Sprintf("Workstream state changed from %s to %s: %s", st.Value, AbandonedState, c.Request.Change)
	}
	x := intake.PinnedMove{
		Repo: repo, Stream: stream, Subject: trace.FeatureSubject, State: st,
		Trigger: intake.TriggerAbandon, Cause: id, Target: AbandonedState, Description: "Abandon the workstream.", Gate: true,
		Ref: cause, ID: id, HeaderCause: cause, Reason: c.Request.Change, Actor: ownerActor, At: s.Now(),
		Events: []trace.Event{trace.Progress(id, "state", body)},
	}.Exit()
	if router.IsSchedulingPath(ctx) {
		s.Router.Submit(x, nil)
		return errAbandonmentRouting
	}
	res, err := s.Router.Route(ctx, x)
	switch {
	case err != nil:
		return err
	case res.Committed:
		return nil
	case res.Hold == router.HoldStale:
		if again, err := repo.Workflow(stream, trace.FeatureSubject); err != nil || again.Value != AbandonedState {
			return errors.Join(err, fmt.Errorf("workstream %s changed while its abandonment was routed", stream))
		}
		return nil
	}
	return fmt.Errorf("the abandonment of workstream %s was held by the router (%s): %s", stream, res.Hold, res.Reason)
}
