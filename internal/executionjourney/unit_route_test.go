package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAnOwnerMoveIsAPinnedMoveThroughTheRouter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-owner-move")
	reported := lastTurn(t, repo, stream, intake.MasonAgent("resume"))
	note := "Split the shadow writer out before the checks run again."
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_owner", Cause: "owner-move-1",
		Default: intake.UnitImplementing, Note: note, Actor: intake.OwnerActor,
		Authority: &router.Authority{Kind: router.AuthorityOwner, Target: intake.UnitImplementing}})
	fixturekit.Must(t, err)
	res, err := f.s.core().RouteExit(ctx, x)
	fixturekit.Must(t, err)
	if !res.Committed || res.Outcome != "followed" || res.Rule != "pinned" || res.State != intake.UnitImplementing {
		t.Fatalf("result = %+v", res)
	}
	state := workflowOf(t, repo, stream, "resume")
	transitions, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	var moved trace.Transition
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject("resume") {
			moved = tr
		}
	}
	if state.Value != intake.UnitImplementing || !execution.IsMove(moved, "resume") || moved.Actor != intake.OwnerActor || moved.DecidedBy != trace.DecidedByOwner || moved.DefaultTo != intake.UnitImplementing || !strings.Contains(moved.Reason, note) {
		t.Fatalf("unit %s after %+v", state.Value, moved)
	}
	if !strings.Contains(intakefixture.NoticeOf(t, repo, stream, moved.ID), "owner moved unit resume from checking to implementing") {
		t.Fatal("the move did not tell the chief of staff")
	}
	ruling, found, err := execution.LatestMasonRuling(repo, stream, "resume")
	fixturekit.Must(t, err)
	if !found || ruling.ResetTurn != reported.Sequence {
		t.Fatalf("clean-turn reset %+v", ruling)
	}

	fixturekit.Must(t, newMasonController(f.s, repo).Pass(ctx))
	turn := lastTurn(t, repo, stream, intake.MasonAgent("resume"))
	if turn.Sequence <= reported.Sequence || !strings.Contains(turn.Request.Prompt, note) || turn.Request.Actor != intake.OwnerActor {
		t.Fatalf("the mason's next turn %s: %s", turn.Request.TurnID, turn.Request.Prompt)
	}
}

func TestARoutedMoveToReviewingRecordsTheCandidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-reviewing")
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_owner", Cause: "owner-move-2",
		Default: intake.UnitReviewing, Note: "Review again.", Actor: intake.OwnerActor,
		Authority: &router.Authority{Kind: router.AuthorityOwner, Target: intake.UnitReviewing}})
	fixturekit.Must(t, err)
	res, err := f.s.core().RouteExit(ctx, x)
	fixturekit.Must(t, err)
	if !res.Committed || res.State != intake.UnitReviewing {
		t.Fatalf("result = %+v", res)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != intake.UnitReviewing {
		t.Fatalf("unit is %s", state.Value)
	}
}
