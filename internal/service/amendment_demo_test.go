package service

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestM4AmendmentDemonstration follows a mason's request through drafting,
// one shed round, the owner gate, recovery, and the next unit turns.
func TestM4AmendmentDemonstration(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{AmendmentApprove, AmendmentReject} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, masons := newParallelMasonFixture(t, 2, 2, parallelPlan)
			defer f.stop(t)
			f.script("amend-1-1", map[string]string{plan.SpecPath: amendedSpec}, nil)
			f.script("amend-1-round-1-"+committeeAgent(1)+"-1", nil, nil)
			f.script("amend-1-reply-1", nil, nil)
			for unit, criterion := range map[string]string{"upload": "spec#1", "audit": "spec#2"} {
				masons.play[masonTurnID(unit)] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
					recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Proof recorded", "criteria": []any{criterionArgs(CriterionReport{Criterion: criterion, Done: "Built " + unit, Evidence: "The planned proof holds", Proof: "fixture proof"})}})
					if err != nil || !recorded {
						return fmt.Errorf("%s done: %s: %v", unit, reason, err)
					}
					return nil
				}
			}
			masons.play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
				_, err := callTool(ctx, tools, questions.AmendTool, map[string]any{"citations": []string{"spec#1"}, "change": "Resume from a durable checkpoint", "reason": "Acknowledgements are not durable"})
				return err
			}
			stream, _ := f.builtAs(t, "amend-demo-"+decision)
			f.awaitUnit(t, stream, "resume", UnitWaiting)
			f.awaitMasonRan(t, stream, "audit")
			f.awaitAmendment(t, stream, amendmentPresented)
			view, err := f.c.Amendment(ctx, stream, "1")
			must(t, err)
			if view.Round != 1 || view.Revision != 1 || !strings.Contains(string(view.Packet), "durable checkpoint") {
				t.Fatalf("owner packet %+v", view)
			}
			before, _, _, err := seal.Latest(f.repository(), stream)
			must(t, err)
			// Reopen the service with a presented round. The recorded objection,
			// packet and owner gate must remain the same across reconciliation.
			f.stop(t)
			f.start(t)
			again, err := f.c.Amendment(ctx, stream, "1")
			must(t, err)
			if again.Round != 1 || again.Revision != 1 || !slices.Equal(again.Packet, view.Packet) {
				t.Fatalf("round duplicated across restart: before %+v, after %+v", view, again)
			}
			_, err = f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: decision, Packet: again.Revision, Note: "Owner ruling for the requester."})
			must(t, err)
			f.awaitAmendment(t, stream, amendmentRuled)
			ruling := f.ruling(t, stream, masonAgent("resume"))
			if len(ruling) != 1 || !strings.Contains(ruling[0].Request.Prompt, "Owner ruling for the requester.") {
				t.Fatalf("requester ruling %+v", ruling)
			}
			after, _, _, err := seal.Latest(f.repository(), stream)
			must(t, err)
			if decision == AmendmentApprove {
				if after.Seal != before.Seal+1 || !slices.Equal(f.revisions(t, stream, plan.SpecDocument), []int{1, 2}) {
					t.Fatalf("approved amendment left seal %+v, spec revisions %v", after, f.revisions(t, stream, plan.SpecDocument))
				}
				f.awaitTransition(t, stream, amendmentUnitID("upload", "1", true), UnitReviewing, UnitImplementing)
			} else if !reflect.DeepEqual(after, before) || !slices.Equal(f.revisions(t, stream, plan.SpecDocument), []int{1}) {
				t.Fatalf("rejection moved sealed documents: before %+v, after %+v", before, after)
			}
			if state, err := f.unitState(stream, "audit"); err != nil || state != UnitReviewing {
				t.Fatalf("unaffected audit unit: %s, %v", state, err)
			}
			if moves := unitMoves(t, f.repository(), stream, "audit"); len(moves) != 0 {
				t.Fatalf("unaffected audit unit was reconsidered: %+v", moves)
			}
			if moved := f.transition(t, stream, trace.UnitSubject("resume")+"_resumed_amendment_1"); moved.To != UnitImplementing {
				t.Fatalf("requester did not resume: %+v", moved)
			}
			masons.check(t)
			f.stop(t)
			f.start(t)
			if turns := f.ruling(t, stream, masonAgent("resume")); len(turns) != 1 {
				t.Fatalf("requester ruling after restart %+v", turns)
			}
		})
	}
}

// TestM4CharterDemonstration starts with a chief-of-staff proposal made from
// an owner ruling and checks the next bundle of another live workstream.
func TestM4CharterDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	defer f.stop(t)
	proposal, err := f.c.CharterProposal(ctx, stream, "1")
	must(t, err)
	if proposal.State != trace.CharterProposed || proposal.Rule != charterRule {
		t.Fatalf("chief's proposal %+v", proposal)
	}
	_, err = f.c.DecideCharter(ctx, stream, "1", CharterDecisionRequest{Decision: trace.CharterRatify, Note: "Make this a standing rule."})
	must(t, err)
	f.settle(t)
	proposal, err = f.c.CharterProposal(ctx, stream, "1")
	must(t, err)
	if proposal.State != trace.CharterChartered || proposal.Charter == 0 {
		t.Fatalf("ratified rule %+v", proposal)
	}
	other := assemble(t, f.s.active.repository, quiet)
	if len(other.CharterNotices) != 1 || other.CharterNotices[0].Rule != charterRule || other.CharterNotices[0].OwnerResponse != charterRuling {
		t.Fatalf("other workstream's next bundle %+v", other.CharterNotices)
	}
}
