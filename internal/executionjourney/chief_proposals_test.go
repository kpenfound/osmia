package executionjourney

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// departingTo scripts a Jev that departs from the default of the chief of
// staff's proposals to target and finds every other exit's default fits.
func departingTo(target string) func(jevfixture.Ask) jevfixture.Reply {
	return func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger == execution.TriggerMoveProposal {
			return jevfixture.Departs(a, target, 0.92, 0.92)
		}
		return jevfixture.DefaultFits(a)
	}
}

// proposalRecords returns the route records of the chief of staff's proposals,
// in the order they were raised.
func proposalRecords(t *testing.T, m *moveFixture) []router.Record {
	t.Helper()
	var out []router.Record
	for _, rec := range routeRecords(t, m.repo, m.stream) {
		if rec.Trigger == execution.TriggerMoveProposal {
			out = append(out, rec)
		}
	}
	return out
}

func assertNoPhraseRequest(t *testing.T, out string) {
	t.Helper()
	for _, banned := range []string{"the owner moves it", "owner_decided", "tell me", "say "} {
		if strings.Contains(out, banned) {
			t.Fatalf("the tool result asks the owner for a phrase (%q): %s", banned, out)
		}
	}
}

// The chief of staff kicks a ready unit: the proposal is the default, so the
// router follows it and the unit re-enters ready with the move recorded. A
// started unit does not go back to ready, and a ready unit whose dependencies
// have merged does not go back to planned.
func TestChiefOfStaffKicksAReadyUnit(t *testing.T) {
	t.Parallel()
	m := newMoveFixture(t, "propose-kick")
	routeThrough(m.f.s, &jevfixture.Transitions{})
	if state := workflowOf(t, m.repo, m.stream, "dedupe"); state.Value != UnitReady {
		t.Fatalf("dedupe is %s, not ready", state.Value)
	}
	out := m.moveUnit(t, `{"unit":"dedupe","to":"ready","note":"Nothing has picked it up; start it."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"by":"chief of staff"`) {
		t.Fatalf("the kick: %s", out)
	}
	moved := latestOf(t, m.repo, m.stream, trace.UnitSubject("dedupe"))
	if moved.From != UnitReady || moved.To != UnitReady || moved.Actor != chiefActor || moved.DecidedBy != trace.DecidedByChief || moved.DefaultTo != UnitReady || moved.Departed() {
		t.Fatalf("the kick's transition %+v", moved)
	}
	recs := proposalRecords(t, m)
	if len(recs) != 1 || recs[0].Unit != "dedupe" || recs[0].Outcome != router.OutcomeFollowed || recs[0].Target != UnitReady || recs[0].Proposed != UnitReady {
		t.Fatalf("route %+v", recs)
	}

	for _, c := range []struct{ input, want string }{
		{`{"unit":"resume","to":"ready","note":"Again."}`, "has started"},
		{`{"unit":"dedupe","to":"planned","note":"Park it."}`, "has merged, so it is ready"},
		{`{"unit":"dedupe","to":"implementing","note":"Revise."}`, "only a started unit"},
	} {
		if out := m.moveUnit(t, c.input); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, c.want) {
			t.Fatalf("%s: %s", c.input, out)
		}
	}
}

// The chief of staff's move takes a unit out of a contest it raised itself, and
// out of one it already ruled on, as its own decision: a pinned move entered with
// no request to Jev and the chief of staff recorded as the decider.
func TestChiefOfStaffMovesAUnitOutOfAContestItRaisedOrRuled(t *testing.T) {
	t.Parallel()
	t.Run("raised", func(t *testing.T) {
		t.Parallel()
		m := newMoveFixture(t, "move-raised")
		p := &jevfixture.Transitions{}
		routeThrough(m.f.s, p)
		if out := m.moveUnit(t, `{"unit":"resume","to":"contested","note":"The reviews keep finding the same gap."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("hold: %s", out)
		}
		if out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"The engine is back; review it."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("the move: %s", out)
		}
		moved := latestOf(t, m.repo, m.stream, trace.UnitSubject("resume"))
		if moved.To != UnitReviewing || moved.DecidedBy != trace.DecidedByChief || moved.Departed() || moved.Judgment != "" {
			t.Fatalf("the move's transition %+v", moved)
		}
		if n := len(p.TransitionRequests()); n != 0 {
			t.Fatalf("Jev got %d transition requests about chief moves", n)
		}
		recs := proposalRecords(t, m)
		if len(recs) != 2 || recs[1].Authority != router.AuthorityChief || recs[1].Outcome != router.OutcomeFollowed {
			t.Fatalf("route %+v", recs)
		}
	})
	t.Run("ruled", func(t *testing.T) {
		t.Parallel()
		m := newMoveFixture(t, "move-ruled")
		p := &jevfixture.Transitions{}
		routeThrough(m.f.s, p)
		m.contest(t)
		if out := m.resolve(t, `{"unit":"resume","decision":"escalate","note":"The checks cannot run here."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("escalate: %s", out)
		}
		if out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"The checks run again; review it."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("the move: %s", out)
		}
		if m.state(t).Value != UnitReviewing {
			t.Fatalf("the unit is %s", m.state(t).Value)
		}
	})
}

// A proposal to approve a candidate never carries the authority to: nothing is
// committed whatever Jev would answer, Jev is not asked, and the move is left
// as a pending owner decision that the result points to.
func TestAChiefProposalToApproveLeavesAPendingOwnerDecision(t *testing.T) {
	t.Parallel()
	m := newMoveFixture(t, "propose-approve")
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(a, UnitApproved, 0.99, 0.99)
	}}
	routeThrough(m.f.s, p)
	out := m.moveUnit(t, `{"unit":"resume","to":"approved","note":"The change holds."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "pending as an owner decision on the recovery card") || !strings.Contains(out, "independent review") {
		t.Fatalf("the proposal to approve: %s", out)
	}
	assertNoPhraseRequest(t, out)
	if m.state(t).Value != UnitReviewing {
		t.Fatalf("the unit is %s", m.state(t).Value)
	}
	for _, tr := range workTransitions(t, m.repo, m.stream) {
		if tr.To == UnitApproved {
			t.Fatalf("a proposal committed an approval: %+v", tr)
		}
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a gate", n)
	}
	recs := proposalRecords(t, m)
	if len(recs) != 1 || recs[0].State != router.StateOwner || recs[0].Hold != router.HoldGate || recs[0].Target != UnitApproved {
		t.Fatalf("route %+v", recs)
	}
	if pending, err := router.Unfinished(m.repo, m.stream); err != nil || len(pending) != 1 {
		t.Fatalf("the pending owner decision %+v %v", pending, err)
	}
}

// A contest the owner moved the unit into is the owner's to move it out of: a
// proposal of the chief of staff leaves a pending owner decision and moves
// nothing, whatever Jev would answer.
func TestAChiefProposalOutOfTheOwnersContestLeavesAPendingOwnerDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMoveFixture(t, "propose-owner-contest")
	p := &jevfixture.Transitions{Script: departingTo(UnitReviewing)}
	routeThrough(m.f.s, p)
	if _, api := m.f.s.ownerMove(ctx, string(m.stream), "resume", UnitMoveRequest{To: UnitContested, Note: "Hold it while I look."}); api != nil {
		t.Fatal(api.Message)
	}
	out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"Review it."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "pending as an owner decision on the recovery card") {
		t.Fatalf("the proposal out of the owner's contest: %s", out)
	}
	assertNoPhraseRequest(t, out)
	if m.state(t).Value != UnitContested {
		t.Fatalf("the unit is %s", m.state(t).Value)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about the owner's hold", n)
	}
}

// A planned unit becomes ready only once every unit it depends on has merged,
// and the chief of staff may hold it as planned, which restarts that stage.
func TestChiefOfStaffMovesAPlannedUnitOnlyWhenItsDependenciesMerged(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, landingPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repo := seedBuild(t, f, "propose-planned", landingPlan, config.WorkspacesGit, base)
	f.s.setSole(&activeProject{repository: repo})
	routeThrough(f.s, &jevfixture.Transitions{})
	scope := intakefixture.ClaimChiefTurn(t, repo, f.project, stream, func() time.Time { return demoStart }, intakefixture.ChiefTurnInput{
		Turn: "events_1", Prompt: "Unit dedupe is planned.", Actor: serviceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.moveUnit(repo, scope)
	if state := workflowOf(t, repo, stream, "dedupe"); state.Value != UnitPlanned {
		t.Fatalf("dedupe is %s, not planned", state.Value)
	}
	if out := intakefixture.Handle(t, tool, `{"unit":"dedupe","to":"ready","note":"Start it."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "depends on has not merged") {
		t.Fatalf("a planned unit became ready before its dependency merged: %s", out)
	}
	if out := intakefixture.Handle(t, tool, `{"unit":"dedupe","to":"planned","note":"Keep it parked until resume lands."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("holding a planned unit: %s", out)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("dedupe"))
	if moved.From != UnitPlanned || moved.To != UnitPlanned || moved.Actor != chiefActor || moved.DecidedBy != trace.DecidedByChief {
		t.Fatalf("the transition %+v", moved)
	}
}
