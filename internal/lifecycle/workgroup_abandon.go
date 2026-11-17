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

// errAbandonmentRouting says the abandonment a cancelled workgroup change asks
// of a member is with the router's worker, which decides it beside the pass.
var errAbandonmentRouting = errors.New("the abandonment is being routed")

// abandonForChange applies the owner's approval of a workgroup cancellation to
// one member: the member's abandonment, an owner gate, is a pinned move of the
// router, which checks the hard prerequisites, commits exactly that target and
// records the owner as its authority. On the scheduling path the exit is handed
// to the router's worker and the pass goes on with the next one.
func (s *Controller) abandonForChange(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, c WorkgroupChange) error {
	if s.Router == nil {
		return errors.New("routing the abandonment of a workstream is unavailable")
	}
	// The router compares the state as the trace holds it, which is not always
	// the state the workstream reads as (a prepared seal reads as ratified).
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
		// The member moved since it was read: abandoned elsewhere is done, any
		// other state is read again by the next pass.
		if again, err := repo.Workflow(stream, trace.FeatureSubject); err != nil || again.Value != AbandonedState {
			return errors.Join(err, fmt.Errorf("workstream %s changed while its abandonment was routed", stream))
		}
		return nil
	}
	return fmt.Errorf("the abandonment of workstream %s was held by the router (%s): %s", stream, res.Hold, res.Reason)
}
