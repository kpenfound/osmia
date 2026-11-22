package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

// orderedRebases returns the unit's rebase operations in the order they were
// asked for.
func orderedRebases(t *testing.T, b *boundedRebase, unit string) []trace.OperationRecord {
	t.Helper()
	ops := rebaseOperations(t, b.repository, b.stream, unit)
	number := func(op trace.OperationRecord) int {
		in, err := decodeRebase(op.Operation)
		must(t, err)
		return in.Rebase
	}
	slices.SortFunc(ops, func(a, c trace.OperationRecord) int { return number(a) - number(c) })
	return ops
}

// A rebase refused by the bounded give-up does not count as done for its
// feature branch commit: the foreman asks for it again on the same commit,
// and once maxRefusedRebases onto it are refused the unit is contested
// instead. The contest is ruled through the ordinary paths, and a unit that
// leaves it gets a fresh rebase.
func TestRefusedRebaseIsAskedForAgainAndContestsTheUnitOnceTheBoundIsUsedUp(t *testing.T) {
	t.Parallel()
	b := newBoundedRebase(t, "refused-rebase-again")
	ctx := context.Background()
	lands := &foreman{masons: newMasonController(b.f.s, b.repository)}
	failing := true
	b.f.s.boundary = func(name string) error {
		if failing && name == "rebase-snapshotting" {
			return errors.New("the clone is unreachable")
		}
		return nil
	}
	for i := 1; i <= maxRefusedRebases; i++ {
		b.pass(t, reconcile.GiveUpFailures+1)
		b.clock.advance(10 * time.Minute)
		b.pass(t, 1)
		ops := orderedRebases(t, b, "resume")
		if len(ops) != i || ops[i-1].Result == nil || ops[i-1].Result.Outcome != "failed" {
			t.Fatalf("after give-up %d resume's rebases are %+v", i, ops)
		}
		must(t, lands.Pass(ctx))
		ops = orderedRebases(t, b, "resume")
		state, err := b.repository.Workflow(b.stream, trace.UnitSubject("resume"))
		must(t, err)
		if i < maxRefusedRebases {
			if len(ops) != i+1 || ops[i].Result != nil {
				t.Fatalf("refused rebase %d was not asked for again: %+v", i, ops)
			}
			if in, err := decodeRebase(ops[i].Operation); err != nil || in.Onto != b.landed {
				t.Fatalf("rebase %d is %+v, not onto %s: %v", i+1, in, b.landed, err)
			}
			if state.Value == UnitContested {
				t.Fatalf("resume was contested after %d refusals", i)
			}
			continue
		}
		if len(ops) != maxRefusedRebases {
			t.Fatalf("the foreman asked again after %d refusals: %+v", i, ops)
		}
		if state.Value != UnitContested {
			t.Fatalf("after %d refusals resume is %s, not contested", i, state.Value)
		}
	}
	contest, _, err := execution.MasonContest(b.repository, b.stream, "resume")
	must(t, err)
	if !rebaseContest(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("refused %d rebases of unit resume's workspace", maxRefusedRebases)) || !strings.Contains(contest.Reason, "the clone is unreachable") {
		t.Fatalf("the contest %+v", contest)
	}
	notices := b.notices(t, "Unit resume was not rebased onto")
	if len(notices) != maxRefusedRebases {
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
	// A contested unit's rebase is not asked for again.
	must(t, lands.Pass(ctx))
	if ops := orderedRebases(t, b, "resume"); len(ops) != maxRefusedRebases {
		t.Fatalf("a contested unit's rebase was asked for again: %+v", ops)
	}

	// The chief of staff's revise ruling takes resume out of its contest, and
	// a move to implementing takes dedupe out of its own; each gets a fresh
	// rebase onto the same commit.
	failing = false
	if _, api := b.f.s.recordContestedRuling(ctx, b.repository, b.stream, "resume", ContestedRulingRequest{Decision: "revise", Note: "The clone is reachable again."}, chiefActor); api != nil {
		t.Fatalf("the chief's ruling: %+v", api)
	}
	if state, err := b.repository.Workflow(b.stream, trace.UnitSubject("dedupe")); err != nil || state.Value != UnitContested {
		t.Fatalf("dedupe is %+v: %v", state, err)
	}
	if _, api := b.f.s.moveUnit(ctx, b.repository, b.stream, "dedupe", UnitMoveRequest{To: UnitImplementing, Note: "The clone is reachable again."}, chiefActor, ""); api != nil {
		t.Fatalf("the chief's move: %+v", api)
	}
	must(t, lands.Pass(ctx))
	for _, unit := range []string{"resume", "dedupe"} {
		if state, err := b.repository.Workflow(b.stream, trace.UnitSubject(unit)); err != nil || state.Value != UnitImplementing {
			t.Fatalf("%s is %+v after its ruling: %v", unit, state, err)
		}
		ops := orderedRebases(t, b, unit)
		if len(ops) != maxRefusedRebases+1 || ops[maxRefusedRebases].Result != nil {
			t.Fatalf("%s got no fresh rebase after its ruling: %+v", unit, ops)
		}
		if in, err := decodeRebase(ops[maxRefusedRebases].Operation); err != nil || in.Onto != b.landed {
			t.Fatalf("%s's fresh rebase is %+v, not onto %s: %v", unit, in, b.landed, err)
		}
	}
	b.pass(t, 1)
	units := newUnitWorkspaces(b.f.s.cfg, b.repository)
	for _, unit := range []string{"resume", "dedupe"} {
		if behind, err := units.behind(ctx, b.stream, unit); err != nil || behind {
			t.Fatalf("%s is behind after its fresh rebase: %v %v", unit, behind, err)
		}
	}
}
