package intakejourney

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// After the debate concluded, the chief of staff's request for a redraft or
// another round is its own decision: a pinned move that asks for the redraft or
// the round with the chief of staff's note recorded, as the owner's command
// records it, and asks Jev nothing.
func TestAChiefRequestAfterTheDebateConcludesIsPinned(t *testing.T) {
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
			asked := &triggersAsked{reply: jevfixture.DefaultFits}
			p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
			f := newDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *Options) { o.JevProvider = p.Factory() })
			defer f.stop(t)
			stream := f.reachJevPacket(t)
			core := f.s.core()
			controller := service.InspectForTest(core).Intake()
			ctx := context.Background()
			note := "The standing dissent needs " + c.name + "."
			x, api := controller.ShedProposalExit(string(stream), intake.ShedProposal{Redraft: c.redraft, Note: note, Actor: intake.ChiefActor, At: time.Now().UTC()})
			if api != nil {
				t.Fatal(api.Message)
			}
			res, err := core.RouteExit(ctx, x)
			must(t, err)
			if !res.Committed || res.Target != c.target || res.Outcome != router.OutcomeFollowed || res.Rule != "pinned" {
				t.Fatalf("the chief's request: %+v", res)
			}
			moved := f.awaitSubjectMove(t, stream, intake.OwnerSubject, c.moved)
			if moved.DecidedBy != trace.DecidedByChief || moved.Departed() || moved.Judgment != "" {
				t.Fatalf("the request's transition %+v", moved)
			}
			rec, ok := f.routeOf(t, stream, intake.TriggerMoveProposal)
			if !ok || rec.Authority != router.AuthorityChief || rec.Actor != intake.ChiefActor {
				t.Fatalf("route %+v (found %v)", rec, ok)
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
