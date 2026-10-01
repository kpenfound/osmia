package service

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// recordActivity commits a transition record for the workstream at the given
// time. Its subject is unique to the record so it never conflicts with an
// earlier one.
func recordActivity(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string, at time.Time) {
	t.Helper()
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: "test-activity"},
		Subject: id, To: "done", Reason: "test activity",
	}})
	must(t, err)
}

// activityFixture opens a fresh project trace under its own home directory,
// without starting a service, so the caller can seed workstreams and
// activity before the first start.
func activityFixture(t *testing.T) (Options, *config.Config, *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "activity-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	demoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, ownerActor)
	must(t, err)
	return opts, cfg, repo
}

// workstreamOrder returns the order in which ids of interest appear in list,
// leaving out anything else the list reports.
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

// The sidebar list is ordered by each workstream's last activity, newest
// first, regardless of the order the workstreams were created in, and a new
// activity record promotes a workstream that was not first to the top the
// next time the list loads.
func TestWorkstreamListOrderedByLastActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg, repo := activityFixture(t)
	first := config.WorkstreamID("w_00000000000000000000000000000001")
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	third := config.WorkstreamID("w_00000000000000000000000000000003")
	// Created oldest to newest, but given activity in the opposite order, so
	// an order that followed creation instead of activity would disagree.
	must(t, repo.CreateWorkstream(ctx, first, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, second, demoStart.Add(time.Second), ownerActor))
	must(t, repo.CreateWorkstream(ctx, third, demoStart.Add(2*time.Second), ownerActor))
	recordActivity(t, repo, third, "third-old", demoStart.Add(3*time.Second))
	recordActivity(t, repo, second, "second-mid", demoStart.Add(10*time.Second))
	recordActivity(t, repo, first, "first-new", demoStart.Add(20*time.Second))
	must(t, repo.Close())

	s, c := start(t, opts)
	list, err := c.Statuses(ctx)
	must(t, err)
	if got, want := workstreamOrder(list, first, second, third), []config.WorkstreamID{first, second, third}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	must(t, s.Close())

	// New activity on third, which is currently last, promotes it to first.
	repo2, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	recordActivity(t, repo2, third, "third-new", demoStart.Add(30*time.Second))
	must(t, repo2.Close())

	_, c = start(t, opts)
	list, err = c.Statuses(ctx)
	must(t, err)
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
	must(t, repo.CreateWorkstream(ctx, older, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, newer, demoStart.Add(time.Second), ownerActor))
	recordActivity(t, repo, older, "older-activity", demoStart.Add(2*time.Second))
	recordActivity(t, repo, newer, "newer-activity", demoStart.Add(20*time.Second))
	must(t, repo.Close())

	s, c := start(t, opts)
	t.Cleanup(func() { s.Close() })
	list, err := c.Statuses(ctx)
	must(t, err)
	want := []config.WorkstreamID{newer, older}
	if got := workstreamOrder(list, older, newer); !slices.Equal(got, want) {
		t.Fatalf("initial order %v, want %v", got, want)
	}
	// Reading the older workstream, alone and in the list, many times after
	// the newer workstream's last activity must not move it to the top: were
	// a read counted as activity, its timestamp would be later than the
	// newer workstream's and would invert the order below.
	for i := 0; i < 20; i++ {
		if _, err := c.Status(ctx, older); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Statuses(ctx); err != nil {
			t.Fatal(err)
		}
	}
	list, err = c.Statuses(ctx)
	must(t, err)
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
	// low and high share both a creation time and a last activity time (each
	// has none beyond its creation), so only the workstream ID orders them.
	low := config.WorkstreamID("w_00000000000000000000000000000011")
	high := config.WorkstreamID("w_00000000000000000000000000000022")
	// old was created before low and high but its one activity record ties
	// their last activity time, so only its older creation time explains why
	// it sorts after them.
	old := config.WorkstreamID("w_00000000000000000000000000000005")
	// newest has the most recent last activity of all four.
	newest := config.WorkstreamID("w_00000000000000000000000000000099")
	tied := demoStart.Add(100 * time.Second)
	must(t, repo.CreateWorkstream(ctx, old, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, low, tied, ownerActor))
	must(t, repo.CreateWorkstream(ctx, high, tied, ownerActor))
	must(t, repo.CreateWorkstream(ctx, newest, demoStart.Add(50*time.Second), ownerActor))
	recordActivity(t, repo, old, "old-activity", tied)
	recordActivity(t, repo, newest, "newest-activity", demoStart.Add(200*time.Second))
	must(t, repo.Close())

	want := []config.WorkstreamID{newest, low, high, old}
	for i := 0; i < 3; i++ {
		s, c := start(t, opts)
		list, err := c.Statuses(ctx)
		must(t, err)
		if got := workstreamOrder(list, old, low, high, newest); !slices.Equal(got, want) {
			t.Fatalf("load %d order %v, want %v", i, got, want)
		}
		must(t, s.Close())
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
	must(t, repo.CreateWorkstream(ctx, a, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, b, demoStart.Add(time.Second), ownerActor))
	must(t, repo.CreateWorkstream(ctx, c1, demoStart.Add(2*time.Second), ownerActor))
	recordActivity(t, repo, a, "a-activity", demoStart.Add(30*time.Second))
	recordActivity(t, repo, b, "b-activity", demoStart.Add(20*time.Second))
	recordActivity(t, repo, c1, "c-activity", demoStart.Add(10*time.Second))
	must(t, repo.Close())

	s, c := start(t, opts)
	before, err := c.Statuses(ctx)
	must(t, err)
	order := workstreamOrder(before, a, b, c1)
	if want := []config.WorkstreamID{a, b, c1}; !slices.Equal(order, want) {
		t.Fatalf("order before restart %v, want %v", order, want)
	}
	must(t, s.Close())

	s2, c2 := start(t, opts)
	t.Cleanup(func() { s2.Close() })
	after, err := c2.Statuses(ctx)
	must(t, err)
	if got := workstreamOrder(after, a, b, c1); !slices.Equal(got, order) {
		t.Fatalf("order after restart %v, want %v", got, order)
	}
}
