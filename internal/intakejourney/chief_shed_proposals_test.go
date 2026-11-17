package intakejourney

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// After the debate concluded, a proposal of the chief of staff for a redraft or
// another round leaves the decision to the owner by default. A Jev that departs
// at the strict bar asks for the redraft or the round with the chief of staff's
// note recorded, as the owner's command records it.
func TestAChiefProposalAfterTheDebateConcludesIsMadeOnlyByADeparture(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		redraft bool
		target  string
		moved   string
		doc     func(int) string
	}{
		{"redraft", true, intake.TargetRedraft, "redraft-1", shed.RedraftDocumentID},
		{"another round", false, intake.TargetDebateRound, "more-1", shed.MoreDocumentID},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var departTo atomic.Pointer[string]
			asked := &triggersAsked{reply: func(ask jevfixture.Ask) jevfixture.Reply {
				if to := departTo.Load(); to != nil && ask.Trigger == intake.TriggerMoveProposal {
					return jevfixture.Departs(ask, *to, 0.96, 0.96)
				}
				return jevfixture.DefaultFits(ask)
			}}
			p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
			f := newDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *Options) { o.JevProvider = p.Factory() })
			defer f.stop(t)
			stream := f.reachJevPacket(t)
			core := f.s.core()
			controller := service.InspectForTest(core).Intake()
			ctx := context.Background()
			note := "The standing dissent needs " + c.name + "."
			propose := func() router.Result {
				x, api := controller.ShedProposalExit(string(stream), intake.ShedProposal{Redraft: c.redraft, Note: note, Actor: intake.ChiefActor, At: time.Now().UTC()})
				if api != nil {
					t.Fatal(api.Message)
				}
				res, err := core.RouteExit(ctx, x)
				must(t, err)
				return res
			}

			before := f.ownerMoves(t, stream)
			res := propose()
			if !res.Committed || res.Target != intake.TargetWait || res.Outcome != router.OutcomeFollowed {
				t.Fatalf("the proposal that followed the default: %+v", res)
			}
			if after := f.ownerMoves(t, stream); !slices.Equal(before, after) {
				t.Fatalf("a followed default moved the owner subject: %v then %v", before, after)
			}
			if docs := f.documents(t, stream, c.doc(1)); len(docs) != 0 {
				t.Fatalf("a followed default recorded the request: %+v", docs)
			}
			rec, ok := f.routeOf(t, stream, intake.TriggerMoveProposal)
			if !ok || rec.Default != intake.TargetWait || rec.Proposed != c.target || !slices.Contains(rec.Offered, c.target) || rec.Actor != intake.ChiefActor {
				t.Fatalf("route %+v (found %v)", rec, ok)
			}

			departTo.Store(&c.target)
			res = propose()
			if !res.Committed || res.Target != c.target || res.Outcome != router.OutcomeDeparted {
				t.Fatalf("the departure: %+v", res)
			}
			moved := f.awaitSubjectMove(t, stream, intake.OwnerSubject, c.moved)
			if moved.DecidedBy != trace.DecidedByJev || moved.DefaultTo != intake.TargetWait || !moved.Departed() || moved.Judgment == "" {
				t.Fatalf("the departure's transition %+v", moved)
			}
			docs := f.documents(t, stream, c.doc(1))
			if len(docs) == 0 {
				t.Fatalf("the request is not recorded: %+v", docs)
			}
			if c.redraft && !strings.Contains(docs[len(docs)-1].Content, note) {
				t.Fatalf("the redraft request lost the chief of staff's note: %s", docs[len(docs)-1].Content)
			}
		})
	}
}
