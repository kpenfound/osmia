package intakejourney

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAManagerRequestAfterTheDebateConcludesIsPinned(t *testing.T) {
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
			f := journey.NewDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *journey.Options) { o.JevProvider = p.Factory() })
			defer f.Stop(t)
			stream := f.ReachJevPacket(t)
			core := f.S.Core()
			controller := service.InspectForTest(core).Intake()
			ctx := context.Background()
			note := "The standing dissent needs " + c.name + "."
			x, apiErr := controller.ShedProposalExit(string(stream), intake.ShedProposal{Redraft: c.redraft, Note: note, Actor: intake.ManagerActor, At: time.Now().UTC()})
			if apiErr != nil {
				t.Fatal(apiErr.Message)
			}
			res, err := core.RouteExit(ctx, x)
			fixturekit.Must(t, err)
			if !res.Committed || res.Target != c.target || res.Outcome != router.OutcomeFollowed || res.Rule != "pinned" {
				t.Fatalf("the manager's request: %+v", res)
			}
			moved := f.AwaitSubjectMove(t, stream, intake.OwnerSubject, c.moved)
			if moved.DecidedBy != trace.DecidedByManager || moved.Departed() || moved.Judgment != "" {
				t.Fatalf("the request's transition %+v", moved)
			}
			rec, ok := f.RouteOf(t, stream, intake.TriggerMoveProposal)
			if !ok || rec.Authority != router.AuthorityManager || rec.Actor != intake.ManagerActor {
				t.Fatalf("route %+v (found %v)", rec, ok)
			}
			docs := f.Documents(t, stream, c.doc(1))
			if len(docs) == 0 {
				t.Fatalf("the request is not recorded: %+v", docs)
			}
			if c.redraft && !strings.Contains(docs[len(docs)-1].Content, note) {
				t.Fatalf("the redraft request lost the manager's note: %s", docs[len(docs)-1].Content)
			}
		})
	}
}
