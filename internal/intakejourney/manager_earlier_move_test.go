package intakejourney

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
)

type routedExits struct {
	router.Port
	exits []router.Exit
}

func (r *routedExits) Route(ctx context.Context, x router.Exit) (router.Result, error) {
	r.exits = append(r.exits, x)
	return r.Port.Route(ctx, x)
}

func TestTheEarlierMoveEvidenceOfAManagerRequestNamesTheManager(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{reply: jevfixture.DefaultFits}
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := journey.NewDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *journey.Options) { o.JevProvider = p.Factory() })
	defer f.Stop(t)
	stream := f.ReachJevPacket(t)
	ctx := context.Background()
	repo := f.Repository()
	content, err := json.Marshal(lifecycle.WorkstreamMove{Move: 1, From: intake.AssembledState, To: intake.BuildingState, Note: "Repair the resume path.", By: trace.ManagerRulerName})
	fixturekit.Must(t, err)
	header := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: lifecycle.WorkstreamMoveDocument(1), Revision: 1, Project: repo.Project(), Workstream: stream, At: time.Now().UTC(), Actor: intake.ManagerActor, Cause: "fixture"}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: header, Path: lifecycle.WorkstreamMovePath(1), Content: string(content) + "\n"}}))

	moves := service.InspectForTest(f.S.Core()).LifecycleController()
	routed := &routedExits{Port: moves.Router}
	moves.Router = routed
	if _, apiErr := moves.MoveWorkstream(ctx, repo, stream, lifecycle.WorkstreamMoveRequest{To: intake.SketchedState, Note: "The standing dissent needs a redraft."}, intake.ManagerActor, "manager-turn-1"); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	var earlier []string
	for _, x := range routed.exits {
		for _, record := range x.Evidence.Records {
			if record.Kind == "earlier move" {
				earlier = append(earlier, record.Text)
			}
		}
	}
	if want := []string{"manager moved the workstream from assembled to building: Repair the resume path."}; !slices.Equal(earlier, want) {
		t.Fatalf("the earlier move evidence is %q, want %q", earlier, want)
	}
}
