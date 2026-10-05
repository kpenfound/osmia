package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/trace"
)

// relayBeekeeperReply is thread.Dispatcher's Relay hook for every
// registered project's chief-of-staff turns. It records the reply of a
// chief-of-staff turn the Beekeeper's message tool triggered, once, in the
// shadow project's trace, attributed to that workstream's chief of staff,
// and never starts a Beekeeper turn. Any other turn, and a turn that has no
// reply to relay, is left alone.
func (s *Service) relayBeekeeperReply(ctx context.Context, t trace.Thread, q trace.QueuedTurn) error {
	if t.Identity.Role != trace.ChiefOfStaff || q.Request.Actor != beekeeperActor || q.Response == nil {
		return nil
	}
	text := q.Response.Result.FinalResponse
	switch q.Status() {
	case "failed", "interrupted":
		text = q.Response.Failure
		if text == "" {
			text = "the turn did not complete"
		}
	}
	if text == "" {
		return nil
	}
	return beekeeper.RecordRelayedReply(ctx, s.shadow, q.Request.Project, q.Request.Workstream, q.Request.ID, text, s.now())
}
