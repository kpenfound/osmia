package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func activityFixture(t *testing.T) (Options, *config.Config, *trace.Repository) {
	t.Helper()
	opts, cfg, repo := fixturekit.Activity(t)
	return testOptions(opts), cfg, repo
}

func workstreamOrder(list StatusResponse, ids ...config.WorkstreamID) []config.WorkstreamID {
	of := map[config.WorkstreamID]bool{}
	for _, id := range ids {
		of[id] = true
	}
	var order []config.WorkstreamID
	for _, w := range list.Workstreams {
		if of[w.Workstream] {
			order = append(order, w.Workstream)
		}
	}
	return order
}

func TestWorkstreamListOrderedByLastActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg, repo := activityFixture(t)
	first := config.WorkstreamID("w_00000000000000000000000000000001")
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	third := config.WorkstreamID("w_00000000000000000000000000000003")
	intakefixture.Must(t, repo.CreateWorkstream(ctx, first, fixturekit.DemoStart, ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, second, fixturekit.DemoStart.Add(time.Second), ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, third, fixturekit.DemoStart.Add(2*time.Second), ownerActor))
	fixturekit.RecordActivity(t, repo, third, "third-old", fixturekit.DemoStart.Add(3*time.Second))
	fixturekit.RecordActivity(t, repo, second, "second-mid", fixturekit.DemoStart.Add(10*time.Second))
	fixturekit.RecordActivity(t, repo, first, "first-new", fixturekit.DemoStart.Add(20*time.Second))
	intakefixture.Must(t, repo.Close())

	s, c := start(t, opts)
	list, err := c.Statuses(ctx)
	intakefixture.Must(t, err)
	if got, want := workstreamOrder(list, first, second, third), []config.WorkstreamID{first, second, third}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	intakefixture.Must(t, s.Close())

	repo2, err := trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	fixturekit.RecordActivity(t, repo2, third, "third-new", fixturekit.DemoStart.Add(30*time.Second))
	intakefixture.Must(t, repo2.Close())

	_, c = start(t, opts)
	list, err = c.Statuses(ctx)
	intakefixture.Must(t, err)
	if got, want := workstreamOrder(list, first, second, third), []config.WorkstreamID{third, first, second}; !slices.Equal(got, want) {
		t.Fatalf("order after new activity %v, want %v", got, want)
	}
}

// Reading a workstream, or the workstream list, over and over never changes
// its last activity, so it never changes the list's order.
func TestReadOnlyWorkstreamRequestsDoNotChangeLastActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, _, repo := activityFixture(t)
	older := config.WorkstreamID("w_00000000000000000000000000000010")
	newer := config.WorkstreamID("w_00000000000000000000000000000020")
	intakefixture.Must(t, repo.CreateWorkstream(ctx, older, fixturekit.DemoStart, ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, newer, fixturekit.DemoStart.Add(time.Second), ownerActor))
	fixturekit.RecordActivity(t, repo, older, "older-activity", fixturekit.DemoStart.Add(2*time.Second))
	fixturekit.RecordActivity(t, repo, newer, "newer-activity", fixturekit.DemoStart.Add(20*time.Second))
	intakefixture.Must(t, repo.Close())

	s, c := start(t, opts)
	t.Cleanup(func() { s.Close() })
	list, err := c.Statuses(ctx)
	intakefixture.Must(t, err)
	want := []config.WorkstreamID{newer, older}
	if got := workstreamOrder(list, older, newer); !slices.Equal(got, want) {
		t.Fatalf("initial order %v, want %v", got, want)
	}
	for i := 0; i < 20; i++ {
		if _, err := c.Status(ctx, older); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Statuses(ctx); err != nil {
			t.Fatal(err)
		}
	}
	list, err = c.Statuses(ctx)
	intakefixture.Must(t, err)
	if got := workstreamOrder(list, older, newer); !slices.Equal(got, want) {
		t.Fatalf("order after reads %v, want %v", got, want)
	}
}

// Workstreams that share a last activity time sort by creation time, newest
// first, and workstreams that also share a creation time sort by workstream
// ID, the same way on every load.
func TestWorkstreamListTieBreakByCreationThenID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, _, repo := activityFixture(t)
	low := config.WorkstreamID("w_00000000000000000000000000000011")
	high := config.WorkstreamID("w_00000000000000000000000000000022")
	old := config.WorkstreamID("w_00000000000000000000000000000005")
	newest := config.WorkstreamID("w_00000000000000000000000000000099")
	tied := fixturekit.DemoStart.Add(100 * time.Second)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, old, fixturekit.DemoStart, ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, low, tied, ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, high, tied, ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, newest, fixturekit.DemoStart.Add(50*time.Second), ownerActor))
	fixturekit.RecordActivity(t, repo, old, "old-activity", tied)
	fixturekit.RecordActivity(t, repo, newest, "newest-activity", fixturekit.DemoStart.Add(200*time.Second))
	intakefixture.Must(t, repo.Close())

	want := []config.WorkstreamID{newest, low, high, old}
	for i := 0; i < 3; i++ {
		s, c := start(t, opts)
		list, err := c.Statuses(ctx)
		intakefixture.Must(t, err)
		if got := workstreamOrder(list, old, low, high, newest); !slices.Equal(got, want) {
			t.Fatalf("load %d order %v, want %v", i, got, want)
		}
		intakefixture.Must(t, s.Close())
	}
}

// The activity order survives a service restart: the same durable records
// produce the same sidebar order after recovery.
func TestWorkstreamActivityOrderSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, _, repo := activityFixture(t)
	a := config.WorkstreamID("w_000000000000000000000000000000a1")
	b := config.WorkstreamID("w_000000000000000000000000000000b2")
	c1 := config.WorkstreamID("w_000000000000000000000000000000c3")
	intakefixture.Must(t, repo.CreateWorkstream(ctx, a, fixturekit.DemoStart, ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, b, fixturekit.DemoStart.Add(time.Second), ownerActor))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, c1, fixturekit.DemoStart.Add(2*time.Second), ownerActor))
	fixturekit.RecordActivity(t, repo, a, "a-activity", fixturekit.DemoStart.Add(30*time.Second))
	fixturekit.RecordActivity(t, repo, b, "b-activity", fixturekit.DemoStart.Add(20*time.Second))
	fixturekit.RecordActivity(t, repo, c1, "c-activity", fixturekit.DemoStart.Add(10*time.Second))
	intakefixture.Must(t, repo.Close())

	s, c := start(t, opts)
	before, err := c.Statuses(ctx)
	intakefixture.Must(t, err)
	order := workstreamOrder(before, a, b, c1)
	if want := []config.WorkstreamID{a, b, c1}; !slices.Equal(order, want) {
		t.Fatalf("order before restart %v, want %v", order, want)
	}
	intakefixture.Must(t, s.Close())

	s2, c2 := start(t, opts)
	t.Cleanup(func() { s2.Close() })
	after, err := c2.Statuses(ctx)
	intakefixture.Must(t, err)
	if got := workstreamOrder(after, a, b, c1); !slices.Equal(got, order) {
		t.Fatalf("order after restart %v, want %v", got, order)
	}
}
