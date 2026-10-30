package intake

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchitectDoesNotRequestDraftAfterConcurrentAbandon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, err := config.Load(fixturekit.ConfigAt(t, t.TempDir()))
	fixturekit.Must(t, err)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, repo.Close()) })
	stream := fixturekit.Stream
	fixturekit.Must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handed", Revision: 1, Project: cfg.Project.ID, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "handed"}
	_, err = repo.SetFeatureState(ctx, h, HandedState, "Owner handed in a feature")
	fixturekit.Must(t, err)
	h.ID, h.Cause = "draft-1-failed", "draft-1"
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: DraftSubject, To: "failed-1", Reason: "Draft failed"}})
	fixturekit.Must(t, err)
	d := &Drafter{Repository: repo, S: &DrafterDependencies{
		Now:                clock.Now,
		DesignStartPending: func(*trace.Repository, config.WorkstreamID) (bool, error) { return false, nil },
		Pausing: func(config.ProjectID, config.WorkstreamID) (runtime.Pause, bool) {
			// Reconcile has read both states, but has not published its request.
			h.ID, h.Cause = AbandonTransition, AbandonTransition
			_, err := repo.SetFeatureState(ctx, h, AbandonedState, "Superseded")
			fixturekit.Must(t, err)
			return runtime.Pause{}, false
		},
	}}
	fixturekit.Must(t, d.Reconcile(ctx, stream))
	state, err := repo.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if state.Value != AbandonedState {
		t.Fatalf("feature state: %+v", state)
	}
	draft, err := repo.Workflow(stream, DraftSubject)
	fixturekit.Must(t, err)
	if draft.Value != "failed-1" {
		t.Fatalf("draft advanced after abandon: %+v", draft)
	}
	entries, err := repo.Outbox(stream)
	fixturekit.Must(t, err)
	for _, entry := range entries {
		if entry.Event.Operation != nil {
			t.Fatalf("operation requested after abandon: %+v", entry)
		}
	}
}
