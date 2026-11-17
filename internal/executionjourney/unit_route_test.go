package executionjourney

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// An owner's move is a pinned move for the router: it commits exactly the
// owner's target with the effects every unit move shares (the move with its
// note and the mason's fresh clean-turn allowance, and a notice to the chief of
// staff), records the owner as the decider and sends Jev no request.
func TestAnOwnerMoveIsAPinnedMoveThroughTheRouter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-owner-move")
	reported := lastTurn(t, repo, stream, masonAgent("resume"))
	note := "Split the shadow writer out before the checks run again."
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_owner", Cause: "owner-move-1",
		Default: UnitImplementing, Note: note, Actor: ownerActor,
		Authority: &router.Authority{Kind: router.AuthorityOwner, Target: UnitImplementing}})
	must(t, err)
	res, err := f.s.core().RouteExit(ctx, x)
	must(t, err)
	if !res.Committed || res.Outcome != "followed" || res.Rule != "pinned" || res.State != UnitImplementing {
		t.Fatalf("result = %+v", res)
	}
	state := workflowOf(t, repo, stream, "resume")
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	var moved trace.Transition
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject("resume") {
			moved = tr
		}
	}
	if state.Value != UnitImplementing || !isMove(moved, "resume") || moved.Actor != ownerActor || moved.DecidedBy != trace.DecidedByOwner || moved.DefaultTo != UnitImplementing || !strings.Contains(moved.Reason, note) {
		t.Fatalf("unit %s after %+v", state.Value, moved)
	}
	if !strings.Contains(noticeOf(t, repo, stream, moved.ID), "owner moved unit resume from checking to implementing") {
		t.Fatal("the move did not tell the chief of staff")
	}
	ruling, found, err := latestMasonRuling(repo, stream, "resume")
	must(t, err)
	if !found || ruling.ResetTurn != reported.Sequence {
		t.Fatalf("clean-turn reset %+v", ruling)
	}

	// The mason's next turn is the one an owner's command gives it.
	must(t, newMasonController(f.s, repo).Pass(ctx))
	turn := lastTurn(t, repo, stream, masonAgent("resume"))
	if turn.Sequence <= reported.Sequence || !strings.Contains(turn.Request.Prompt, note) || turn.Request.Actor != ownerActor {
		t.Fatalf("the mason's next turn %s: %s", turn.Request.TurnID, turn.Request.Prompt)
	}
}

// A move to reviewing keeps the unit's recorded candidate, as the owner's
// command does, and the unit is reviewed again.
func TestARoutedMoveToReviewingRecordsTheCandidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-reviewing")
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_owner", Cause: "owner-move-2",
		Default: UnitReviewing, Note: "Review again.", Actor: ownerActor,
		Authority: &router.Authority{Kind: router.AuthorityOwner, Target: UnitReviewing}})
	must(t, err)
	res, err := f.s.core().RouteExit(ctx, x)
	must(t, err)
	if !res.Committed || res.State != UnitReviewing {
		t.Fatalf("result = %+v", res)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != UnitReviewing {
		t.Fatalf("unit is %s", state.Value)
	}
}
