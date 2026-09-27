package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const second config.ProjectID = "p_00000000000000000000000000000002"

// twoProjects creates the traces of two projects under one root, each with
// three queued mason turns and one queued reviewer turn, and a scheduler for
// each drawing on one shared pool.
func twoProjects(t *testing.T, limits *config.Capacity) (repos [2]*trace.Repository, schedulers [2]*Scheduler) {
	t.Helper()
	base := t.TempDir()
	shared := &Shared{Traces: func() []*trace.Repository { return repos[:] }}
	for i, id := range []config.ProjectID{project, second} {
		f, repo := setupIn(t, base, id, "mason1", "mason2", "mason3")
		t.Cleanup(func() { repo.Close() })
		f.thread(t, repo, stream, "reviewer1", "reviewer")
		for _, agent := range []string{"mason1", "mason2", "mason3", "reviewer1"} {
			f.queue(t, repo, agent, "one")
		}
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
	slots, err := schedulers[1].Slots(func(Candidate) (bool, error) { return false, nil })
	must(t, err)
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
