package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

func orderedRebases(t *testing.T, b *boundedRebase, unit string) []trace.OperationRecord {
	t.Helper()
	ops := journey.RebaseOperations(t, b.repository, b.stream, unit)
	number := func(op trace.OperationRecord) int {
		in, err := execution.DecodeRebase(op.Operation)
		fixturekit.Must(t, err)
		return in.Rebase
	}
	slices.SortFunc(ops, func(a, c trace.OperationRecord) int { return number(a) - number(c) })
	return ops
}

func TestRefusedRebaseIsAskedForAgainAndContestsTheUnitOnceTheBoundIsUsedUp(t *testing.T) {
	t.Parallel()
	b := newBoundedRebase(t, "refused-rebase-again")
	ctx := context.Background()
	lands := &journey.Foreman{Masons: journey.NewMasonController(b.f.S, b.repository)}
	failing := true
	b.f.S.Boundary = func(name string) error {
		if failing && name == "rebase-snapshotting" {
			return errors.New("the clone is unreachable")
		}
		return nil
	}
	for i := 1; i <= execution.MaxRefusedRebases; i++ {
		b.pass(t, reconcile.GiveUpFailures+1)
		b.clock.Advance(10 * time.Minute)
		b.pass(t, 1)
		ops := orderedRebases(t, b, "resume")
		if len(ops) != i || ops[i-1].Result == nil || ops[i-1].Result.Outcome != "failed" {
			t.Fatalf("after give-up %d resume's rebases are %+v", i, ops)
		}
		fixturekit.Must(t, lands.Pass(ctx))
		ops = orderedRebases(t, b, "resume")
		state, err := b.repository.Workflow(b.stream, trace.UnitSubject("resume"))
		fixturekit.Must(t, err)
		if i < execution.MaxRefusedRebases {
			if len(ops) != i+1 || ops[i].Result != nil {
				t.Fatalf("refused rebase %d was not asked for again: %+v", i, ops)
			}
			if in, err := execution.DecodeRebase(ops[i].Operation); err != nil || in.Onto != b.landed {
				t.Fatalf("rebase %d is %+v, not onto %s: %v", i+1, in, b.landed, err)
			}
			if state.Value == intake.UnitContested {
				t.Fatalf("resume was contested after %d refusals", i)
			}
			continue
		}
		if len(ops) != execution.MaxRefusedRebases {
			t.Fatalf("the foreman asked again after %d refusals: %+v", i, ops)
		}
		if state.Value != intake.UnitContested {
			t.Fatalf("after %d refusals resume is %s, not contested", i, state.Value)
		}
	}
	contest, _, err := execution.MasonContest(b.repository, b.stream, "resume")
	fixturekit.Must(t, err)
	if !execution.RebaseContest(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("refused %d rebases of unit resume's workspace", execution.MaxRefusedRebases)) || !strings.Contains(contest.Reason, "the clone is unreachable") {
		t.Fatalf("the contest %+v", contest)
	}
	notices := b.notices(t, "Unit resume was not rebased onto")
	if len(notices) != execution.MaxRefusedRebases {
		t.Fatalf("refusal notices %q", notices)
	}
	for _, n := range notices {
		if strings.Contains(n, "router to decide") || !strings.Contains(n, "asks for the rebase again on a later pass") {
			t.Fatalf("the refusal notice %q", n)
		}
	}
	if got := b.notices(t, "Unit resume is contested"); len(got) != 1 {
		t.Fatalf("contest notices %q", got)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	if ops := orderedRebases(t, b, "resume"); len(ops) != execution.MaxRefusedRebases {
		t.Fatalf("a contested unit's rebase was asked for again: %+v", ops)
	}

	failing = false
	if _, apiErr := recordContestedRuling(b.f.S, ctx, b.repository, b.stream, "resume", api.ContestedRulingRequest{Decision: "revise", Note: "The clone is reachable again."}, intake.ChiefActor); apiErr != nil {
		t.Fatalf("the chief's ruling: %+v", apiErr)
	}
	if state, err := b.repository.Workflow(b.stream, trace.UnitSubject("dedupe")); err != nil || state.Value != intake.UnitContested {
		t.Fatalf("dedupe is %+v: %v", state, err)
	}
	if _, apiErr := b.f.S.Execution().MoveUnit(ctx, b.repository, b.stream, "dedupe", (api.UnitMoveRequest{To: intake.UnitImplementing, Note: "The clone is reachable again."}), intake.ChiefActor, ""); apiErr != nil {
		t.Fatalf("the chief's move: %+v", apiErr)
	}
	fixturekit.Must(t, lands.Pass(ctx))
	for _, unit := range []string{"resume", "dedupe"} {
		if state, err := b.repository.Workflow(b.stream, trace.UnitSubject(unit)); err != nil || state.Value != intake.UnitImplementing {
			t.Fatalf("%s is %+v after its ruling: %v", unit, state, err)
		}
		ops := orderedRebases(t, b, unit)
		if len(ops) != execution.MaxRefusedRebases+1 || ops[execution.MaxRefusedRebases].Result != nil {
			t.Fatalf("%s got no fresh rebase after its ruling: %+v", unit, ops)
		}
		if in, err := execution.DecodeRebase(ops[execution.MaxRefusedRebases].Operation); err != nil || in.Onto != b.landed {
			t.Fatalf("%s's fresh rebase is %+v, not onto %s: %v", unit, in, b.landed, err)
		}
	}
	b.pass(t, 1)
	units := journey.NewUnitWorkspaces(b.f.S.Cfg, b.repository)
	for _, unit := range []string{"resume", "dedupe"} {
		if behind, err := units.Driver().Behind(ctx, b.stream, unit); err != nil || behind {
			t.Fatalf("%s is behind after its fresh rebase: %v %v", unit, behind, err)
		}
	}
}
