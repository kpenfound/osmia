package scheduler

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const second config.ProjectID = "p_00000000000000000000000000000002"

// twoProjects creates the traces of two projects under one root, each with
// three queued mason turns and one queued reviewer turn, and a scheduler for
// each drawing on one shared pool that both projects have joined.
func twoProjects(t *testing.T, limits *config.Capacity) (repos [2]*trace.Repository, schedulers [2]*Scheduler) {
	t.Helper()
	base := t.TempDir()
	shared := &Shared{}
	for i, id := range []config.ProjectID{project, second} {
		f, repo := setupIn(t, base, id, "mason1", "mason2", "mason3")
		t.Cleanup(func() { repo.Close() })
		f.thread(t, repo, stream, "reviewer1", "reviewer")
		for _, agent := range []string{"mason1", "mason2", "mason3", "reviewer1"} {
			f.queue(t, repo, agent, "one")
		}
		must(t, shared.Join(repo))
		s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits, Shared: shared})
		must(t, err)
		repos[i], schedulers[i] = repo, s
	}
	return repos, schedulers
}

func TestSharedPoolHoldsRoleCapacityAcrossProjects(t *testing.T) {
	ctx := context.Background()
	repos, schedulers := twoProjects(t, &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 3})
	must(t, schedulers[0].Pass(ctx))
	if got := counts(dispatched(t, repos[0])); got["mason"] != 2 || got["reviewer"] != 1 {
		t.Fatalf("first project dispatched %v", got)
	}
	// Every mason and reviewer slot is taken by the first project's turns.
	must(t, schedulers[1].Pass(ctx))
	if got := dispatched(t, repos[1]); len(got) != 0 {
		t.Fatalf("second project dispatched %v past the shared capacity", got)
	}
	slots := schedulers[1].options.Shared.Slots([]config.ProjectID{second})
	if slots.Used["mason"] != 2 || slots.Used["reviewer"] != 1 || len(slots.Waiting) != 4 {
		t.Fatalf("second project's slots: %+v", slots)
	}
	for _, w := range slots.Waiting {
		if w.Project != second || w.Reason != WaitCapacity {
			t.Fatalf("waiting turn %+v", w)
		}
	}
}

func TestSharedPoolHoldsUnderConcurrentPassesOfProjects(t *testing.T) {
	repos, schedulers := twoProjects(t, &config.Capacity{Masons: 3, Reviewers: 1, Committee: 1, PerWorkstream: 3})
	var wg sync.WaitGroup
	for range 4 {
		for _, s := range schedulers {
			wg.Go(func() {
				if err := s.Pass(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	total := map[string]int{}
	for _, repo := range repos {
		for role, n := range counts(dispatched(t, repo)) {
			total[role] += n
		}
	}
	if total["mason"] != 3 || total["reviewer"] != 1 {
		t.Fatalf("dispatched across projects %v; want 3 masons and 1 reviewer", total)
	}
}

func TestPerWorkstreamCapStaysWithTheProject(t *testing.T) {
	ctx := context.Background()
	// The role slots are wide, so only each workstream's own cap binds: a
	// workstream of one project does not count against the other's.
	repos, schedulers := twoProjects(t, &config.Capacity{Masons: 10, Reviewers: 10, Committee: 10, PerWorkstream: 2})
	for i, s := range schedulers {
		must(t, s.Pass(ctx))
		if got := dispatched(t, repos[i]); len(got) != 2 {
			t.Fatalf("project %d dispatched %v; want its workstream's 2", i, got)
		}
	}
}

func TestSharedSlotsListTheWaitingTurnsInProjectOrder(t *testing.T) {
	ctx := context.Background()
	// One mason and one reviewer slot: the first project's pass takes them,
	// so every turn of the second project waits.
	_, schedulers := twoProjects(t, &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 3})
	must(t, schedulers[0].Pass(ctx))
	must(t, schedulers[1].Pass(ctx))
	slots := schedulers[0].options.Shared.Slots([]config.ProjectID{second, project})
	if slots.Used["mason"] != 1 || slots.Used["reviewer"] != 1 {
		t.Fatalf("used %v", slots.Used)
	}
	var order []config.ProjectID
	for _, w := range slots.Waiting {
		order = append(order, w.Project)
	}
	if want := []config.ProjectID{second, second, second, second, project, project}; !slices.Equal(order, want) {
		t.Fatalf("waiting by project %v, want %v", order, want)
	}
	// Only the listed projects' waiting turns are reported.
	if got := schedulers[0].options.Shared.Slots([]config.ProjectID{project}); len(got.Waiting) != 2 || got.Used["mason"] != 1 {
		t.Fatalf("first project's slots: %+v", got)
	}
}

func TestPassCountsAnotherProjectFromItsPublishedTurnsNotItsTrace(t *testing.T) {
	ctx := context.Background()
	repos, schedulers := twoProjects(t, &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 3})
	must(t, schedulers[1].Pass(ctx))
	// The second project's trace can no longer be read at all, as when its
	// storage changes under it; the first project's pass reads only its own.
	must(t, repos[1].Close())
	must(t, schedulers[0].Pass(ctx))
	if got := dispatched(t, repos[0]); len(got) != 0 {
		t.Fatalf("first project dispatched %v into the slots the second project published as taken", got)
	}
	slots := schedulers[0].options.Shared.Slots([]config.ProjectID{project})
	if slots.Used["mason"] != 2 || slots.Used["reviewer"] != 1 || len(slots.Waiting) != 4 {
		t.Fatalf("first project's slots: %+v", slots)
	}
}

func TestJoinCountsTurnsInFlightBeforeTheProjectsFirstPass(t *testing.T) {
	ctx := context.Background()
	limits := &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 3}
	repos, schedulers := twoProjects(t, limits)
	must(t, schedulers[1].Pass(ctx))
	// A restarted service starts a fresh pool, which every project joins
	// before any project passes. The second project's turns are still in
	// flight, to be recovered.
	restarted := &Shared{}
	for _, repo := range repos {
		must(t, restarted.Join(repo))
	}
	first, err := New(repos[0], Options{Now: schedulers[0].options.Now, Capacity: limits, Shared: restarted})
	must(t, err)
	must(t, first.Pass(ctx))
	if got := dispatched(t, repos[0]); len(got) != 0 {
		t.Fatalf("first project dispatched %v past the second project's recovered turns", got)
	}
}

func TestLeaveFreesTheProjectsSlots(t *testing.T) {
	ctx := context.Background()
	repos, schedulers := twoProjects(t, &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 3})
	must(t, schedulers[1].Pass(ctx))
	schedulers[1].options.Shared.Leave(second)
	must(t, schedulers[0].Pass(ctx))
	if got := counts(dispatched(t, repos[0])); got["mason"] != 2 || got["reviewer"] != 1 {
		t.Fatalf("first project dispatched %v once the second project left the pool", got)
	}
}

func TestSharedAnnouncesOnlyChangedSlots(t *testing.T) {
	ctx := context.Background()
	_, schedulers := twoProjects(t, &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 3})
	shared := schedulers[0].options.Shared
	changes := 0
	shared.Changed = func() { changes++ }
	// The first pass dispatches and leaves a turn waiting.
	must(t, schedulers[0].Pass(ctx))
	if changes != 1 {
		t.Fatalf("%d announcements after the first pass, want 1", changes)
	}
	// A pass that finds the same turns in flight and waiting announces
	// nothing.
	must(t, schedulers[0].Pass(ctx))
	if changes != 1 {
		t.Fatalf("%d announcements after an unchanged pass, want 1", changes)
	}
	// The second project's pass finds every slot taken: its turns now wait.
	must(t, schedulers[1].Pass(ctx))
	if changes != 2 {
		t.Fatalf("%d announcements after the second project's pass, want 2", changes)
	}
	shared.Leave(second)
	shared.Leave(second)
	if changes != 3 {
		t.Fatalf("%d announcements after the second project left once, want 3", changes)
	}
}
