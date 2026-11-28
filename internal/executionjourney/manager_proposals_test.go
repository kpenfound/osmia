package executionjourney

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func departingTo(target string) func(jevfixture.Ask) jevfixture.Reply {
	return func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger == execution.TriggerMoveProposal {
			return jevfixture.Departs(a, target, 0.92, 0.92)
		}
		return jevfixture.DefaultFits(a)
	}
}

func proposalRecords(t *testing.T, m *journey.MoveFixture) []router.Record {
	t.Helper()
	var out []router.Record
	for _, rec := range journey.RouteRecords(t, m.Repo, m.Stream) {
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

func TestManagerKicksAReadyUnit(t *testing.T) {
	t.Parallel()
	m := journey.NewMoveFixture(t, "propose-kick")
	journey.RouteThrough(m.F.S, &jevfixture.Transitions{})
	if state := intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "dedupe"); state.Value != intake.UnitReady {
		t.Fatalf("dedupe is %s, not ready", state.Value)
	}
	out := intakefixture.Handle(t, m.Move, `{"unit":"dedupe","to":"ready","note":"Nothing has picked it up; start it."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"by":"manager"`) {
		t.Fatalf("the kick: %s", out)
	}
	moved := latestOf(t, m.Repo, m.Stream, trace.UnitSubject("dedupe"))
	if moved.From != intake.UnitReady || moved.To != intake.UnitReady || moved.Actor != intake.ManagerActor || moved.DecidedBy != trace.DecidedByManager || moved.DefaultTo != intake.UnitReady || moved.Departed() {
		t.Fatalf("the kick's transition %+v", moved)
	}
	recs := proposalRecords(t, m)
	if len(recs) != 1 || recs[0].Unit != "dedupe" || recs[0].Outcome != router.OutcomeFollowed || recs[0].Target != intake.UnitReady || recs[0].Proposed != intake.UnitReady {
		t.Fatalf("route %+v", recs)
	}

	for _, c := range []struct{ input, want string }{
		{`{"unit":"resume","to":"ready","note":"Again."}`, "has started"},
		{`{"unit":"dedupe","to":"planned","note":"Park it."}`, "has merged, so it is ready"},
		{`{"unit":"dedupe","to":"implementing","note":"Revise."}`, "only a started unit"},
	} {
		if out := intakefixture.Handle(t, m.Move, c.input); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, c.want) {
			t.Fatalf("%s: %s", c.input, out)
		}
	}
}

func TestManagerMovesAUnitOutOfAContestItRaisedOrRuled(t *testing.T) {
	t.Parallel()
	t.Run("raised", func(t *testing.T) {
		t.Parallel()
		m := journey.NewMoveFixture(t, "move-raised")
		p := &jevfixture.Transitions{}
		journey.RouteThrough(m.F.S, p)
		if out := intakefixture.Handle(t, m.Move, `{"unit":"resume","to":"contested","note":"The reviews keep finding the same gap."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("hold: %s", out)
		}
		if out := intakefixture.Handle(t, m.Move, `{"unit":"resume","to":"reviewing","note":"The engine is back; review it."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("the move: %s", out)
		}
		moved := latestOf(t, m.Repo, m.Stream, trace.UnitSubject("resume"))
		if moved.To != intake.UnitReviewing || moved.DecidedBy != trace.DecidedByManager || moved.Departed() || moved.Judgment != "" {
			t.Fatalf("the move's transition %+v", moved)
		}
		if n := len(p.TransitionRequests()); n != 0 {
			t.Fatalf("Jev got %d transition requests about manager moves", n)
		}
		recs := proposalRecords(t, m)
		if len(recs) != 2 || recs[1].Authority != router.AuthorityManager || recs[1].Outcome != router.OutcomeFollowed {
			t.Fatalf("route %+v", recs)
		}
	})
	t.Run("ruled", func(t *testing.T) {
		t.Parallel()
		m := journey.NewMoveFixture(t, "move-ruled")
		p := &jevfixture.Transitions{}
		journey.RouteThrough(m.F.S, p)
		m.Contest(t)
		if out := m.Resolve(t, `{"unit":"resume","decision":"escalate","note":"The checks cannot run here."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("escalate: %s", out)
		}
		if out := intakefixture.Handle(t, m.Move, `{"unit":"resume","to":"reviewing","note":"The checks run again; review it."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("the move: %s", out)
		}
		if intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "resume").Value != intake.UnitReviewing {
			t.Fatalf("the unit is %s", intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "resume").Value)
		}
	})
}

func TestAManagerProposalToApproveLeavesAPendingOwnerDecision(t *testing.T) {
	t.Parallel()
	m := journey.NewMoveFixture(t, "propose-approve")
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(a, intake.UnitApproved, 0.99, 0.99)
	}}
	journey.RouteThrough(m.F.S, p)
	out := intakefixture.Handle(t, m.Move, `{"unit":"resume","to":"approved","note":"The change holds."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "pending as an owner decision on the recovery card") || !strings.Contains(out, "independent review") {
		t.Fatalf("the proposal to approve: %s", out)
	}
	assertNoPhraseRequest(t, out)
	if intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "resume").Value != intake.UnitReviewing {
		t.Fatalf("the unit is %s", intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "resume").Value)
	}
	for _, tr := range intakefixture.WorkTransitions(t, m.Repo, m.Stream) {
		if tr.To == intake.UnitApproved {
			t.Fatalf("a proposal committed an approval: %+v", tr)
		}
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a gate", n)
	}
	recs := proposalRecords(t, m)
	if len(recs) != 1 || recs[0].State != router.StateOwner || recs[0].Hold != router.HoldGate || recs[0].Target != intake.UnitApproved {
		t.Fatalf("route %+v", recs)
	}
	if pending, err := router.Unfinished(m.Repo, m.Stream); err != nil || len(pending) != 1 {
		t.Fatalf("the pending owner decision %+v %v", pending, err)
	}
}

func TestAManagerProposalOutOfTheOwnersContestLeavesAPendingOwnerDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := journey.NewMoveFixture(t, "propose-owner-contest")
	p := &jevfixture.Transitions{Script: departingTo(intake.UnitReviewing)}
	journey.RouteThrough(m.F.S, p)
	if _, apiErr := m.F.S.Execution().OwnerMove(ctx, string(m.Stream), "resume", (api.UnitMoveRequest{To: intake.UnitContested, Note: "Hold it while I look."})); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	out := intakefixture.Handle(t, m.Move, `{"unit":"resume","to":"reviewing","note":"Review it."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "pending as an owner decision on the recovery card") {
		t.Fatalf("the proposal out of the owner's contest: %s", out)
	}
	assertNoPhraseRequest(t, out)
	if intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "resume").Value != intake.UnitContested {
		t.Fatalf("the unit is %s", intakefixture.UnitWorkflow(t, m.Repo, m.Stream, "resume").Value)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about the owner's hold", n)
	}
}

func TestManagerMovesAPlannedUnitOnlyWhenItsDependenciesMerged(t *testing.T) {
	t.Parallel()
	f, _ := journey.NewMasonFixture(t, 1, landingPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repo := journey.SeedBuild(t, f, "propose-planned", landingPlan, config.WorkspacesGit, base)
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	journey.RouteThrough(f.S, &jevfixture.Transitions{})
	scope := intakefixture.ClaimManagerTurn(t, repo, f.Project, stream, func() time.Time { return fixturekit.DemoStart }, intakefixture.ManagerTurnInput{
		Turn: "events_1", Prompt: "Unit dedupe is planned.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.S)
	tool := controls.Execution().MoveUnit(repo, scope)
	if state := intakefixture.UnitWorkflow(t, repo, stream, "dedupe"); state.Value != intake.UnitPlanned {
		t.Fatalf("dedupe is %s, not planned", state.Value)
	}
	if out := intakefixture.Handle(t, tool, `{"unit":"dedupe","to":"ready","note":"Start it."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "depends on has not merged") {
		t.Fatalf("a planned unit became ready before its dependency merged: %s", out)
	}
	if out := intakefixture.Handle(t, tool, `{"unit":"dedupe","to":"planned","note":"Keep it parked until resume lands."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("holding a planned unit: %s", out)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("dedupe"))
	if moved.From != intake.UnitPlanned || moved.To != intake.UnitPlanned || moved.Actor != intake.ManagerActor || moved.DecidedBy != trace.DecidedByManager {
		t.Fatalf("the transition %+v", moved)
	}
}
