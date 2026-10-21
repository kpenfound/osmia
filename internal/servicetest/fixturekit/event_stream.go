package fixturekit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// EventStream reads c's event stream until the test ends, and returns the
// events received and a channel that yields Events' result once it returns.
func EventStream(t *testing.T, c interface {
	Events(context.Context, func(api.Event) error) error
}) (<-chan api.Event, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events, done := make(chan api.Event, 1024), make(chan error, 1)
	go func() {
		done <- c.Events(ctx, func(e api.Event) error {
			events <- e
			return nil
		})
	}()
	t.Cleanup(cancel)
	return events, done
}

// NextEvent returns the stream's next event.
func NextEvent(t *testing.T, events <-chan api.Event) api.Event {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
		return api.Event{}
	}
}

// AwaitEvents reads the stream until it has seen every one of want, in any
// order, and returns everything it read.
func AwaitEvents(t *testing.T, events <-chan api.Event, want ...api.Event) []api.Event {
	t.Helper()
	var seen []api.Event
	for missing := slices.Clone(want); len(missing) > 0; {
		e := NextEvent(t, events)
		seen = append(seen, e)
		missing = slices.DeleteFunc(missing, func(w api.Event) bool { return w == e })
	}
	return seen
}

// SeedEventTrace plants a thread with one question escalated to the owner.
func SeedEventTrace(t *testing.T, cfg *config.Config) {
	t.Helper()
	ctx := context.Background()
	repo, err := trace.Open(cfg.Root, cfg.Project)
	Must(t, err)
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: DemoAgent, Project: Project, Workstream: Quiet, At: DemoStart, Actor: OwnerActor, Cause: "workstream_created"}, Role: DemoRole, ThreadID: DemoThread}
	Must(t, repo.CreateThread(ctx, identity))
	sessions := t.TempDir()
	_, err = repo.Ask(ctx, DemoAgent, ClaimTurn(t, repo, sessions, Quiet, DemoAgent, DemoThread, "build", DemoStart), "Where does state live?", DemoStart)
	Must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, Quiet, DemoStart, ServiceActor)
	Must(t, err)
	_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, ClaimTurn(t, repo, sessions, Quiet, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", DemoStart),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Recommendation: "In files."}, DemoStart)
	Must(t, err)
	Must(t, repo.Close())
}
