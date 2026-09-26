package scheduler

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const otherProject config.ProjectID = "p_00000000000000000000000000000002"

// masons creates a second workstream and, in each workstream, a mason thread
// per ID with one queued turn.
func masons(t *testing.T, f *fixture, repo *trace.Repository, ids map[config.WorkstreamID][]string) {
	t.Helper()
	must(t, repo.CreateWorkstream(context.Background(), other, f.clock.Now(), owner))
	for _, ws := range []config.WorkstreamID{stream, other} {
		for _, id := range ids[ws] {
			f.thread(t, repo, ws, id, "mason")
			f.queueOn(t, repo, ws, id, "one")
		}
	}
}

// running records the threads whose turns ran, in order.
type running struct {
	mu  sync.Mutex
	ran []string
}

func (r *running) turns() coreadapter.Turns {
	return turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ran = append(r.ran, strings.TrimPrefix(p.Scope.Thread, "thread_"))
		return result(p.Scope.Turn).Value, nil
	})
}

func (r *running) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ran)
}

func TestFreedSlotGoesToTheEarliestStage(t *testing.T) {
	t.Parallel()
	stages := []string{"reviewer", "mason", "committee", "architect"}
	for i, want := range stages {
		t.Run(want, func(t *testing.T) {
			// Every later stage competes for the workstream's only slot, queued
			// ahead of it in the trace.
			f, repo := setup(t)
			defer repo.Close()
			for _, role := range slices.Backward(stages[i:]) {
				f.thread(t, repo, stream, role+"1", role)
				f.queue(t, repo, role+"1", "one")
			}
			s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 5, Reviewers: 5, Committee: 5, PerWorkstream: 1}})
			must(t, err)
			must(t, s.Pass(context.Background()))
			if got := dispatched(t, repo); !slices.Equal(got, []string{want + "1/one"}) {
				t.Fatalf("dispatched %v, want %s first", got, want)
			}
		})
	}
}

func TestRuntimePriorityWinsWithinAStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		priorities []runtime.Priority
		want       string
	}{
		{"first named", []runtime.Priority{{Project: project, Workstreams: []config.WorkstreamID{other, stream}}}, "b1/one"},
		{"named before unnamed", []runtime.Priority{{Project: project, Workstreams: []config.WorkstreamID{other}}}, "b1/one"},
		{"other project's order", []runtime.Priority{{Project: otherProject, Workstreams: []config.WorkstreamID{other}}}, "a1/one"},
		{"no order", nil, "a1/one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := setup(t)
			defer repo.Close()
			masons(t, f, repo, map[config.WorkstreamID][]string{stream: {"a1"}, other: {"b1"}})
			// The lower-priority workstream's review still goes before either mason.
			f.thread(t, repo, stream, "reviewer1", "reviewer")
			f.queue(t, repo, "reviewer1", "one")
			var offered []string
			s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5},
				Priorities: func() []runtime.Priority { return tc.priorities },
				Admit: func(_ context.Context, c Candidate) (bool, error) {
					if c.Project != project {
						t.Errorf("candidate project %s", c.Project)
					}
					offered = append(offered, c.Thread.Identity.ID)
					return true, nil
				}})
			must(t, err)
			must(t, s.Pass(ctx))
			if len(offered) != 2 || offered[0] != "reviewer1" || offered[1]+"/one" != tc.want {
				t.Fatalf("offered %v, want reviewer1 then %s", offered, tc.want)
			}
			if got := dispatched(t, repo, stream, other); !slices.Contains(got, tc.want) || len(got) != 2 {
				t.Fatalf("dispatched %v, want %s", got, tc.want)
			}
		})
	}
}

func TestEqualPriorityWorkstreamsTakeSlotsInTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ids := map[config.WorkstreamID][]string{stream: {"a1", "a2", "a3"}, other: {"b1", "b2", "b3"}}

	// Within one pass, the slots alternate between the workstreams.
	f, repo := setup(t)
	defer repo.Close()
	masons(t, f, repo, ids)
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 4, Reviewers: 1, Committee: 1, PerWorkstream: 5}})
	must(t, err)
	must(t, s.Pass(ctx))
	if got, want := dispatched(t, repo, stream, other), []string{"a1/one", "a2/one", "b1/one", "b2/one"}; !slices.Equal(got, want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}

	// Across passes, each freed slot goes to the other workstream.
	f, repo = setup(t)
	defer repo.Close()
	masons(t, f, repo, ids)
	r := &running{}
	c := f.controllerWith(t, repo, r.turns(), Options{Capacity: &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}})
	for range 6 {
		must(t, c.Pass(ctx))
	}
	if got, want := r.got(), []string{"a1", "b1", "a2", "b2", "a3", "b3"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
}

func TestRotationContinuesAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, repo := setup(t)
	masons(t, f, repo, map[config.WorkstreamID][]string{stream: {"a1", "a2"}, other: {"b1", "b2"}})
	limits := Options{Capacity: &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}}
	r := &running{}
	must(t, f.controllerWith(t, repo, r.turns(), limits).Pass(ctx))
	// A fresh scheduler reads the rotation from the trace: the workstream that
	// has not had a turn goes next, not the one first by ID.
	repo = f.reopen(t, repo)
	defer func() { repo.Close() }()
	c := f.controllerWith(t, repo, r.turns(), limits)
	must(t, c.Pass(ctx))
	repo = f.reopen(t, repo)
	c = f.controllerWith(t, repo, r.turns(), limits)
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if got, want := r.got(), []string{"a1", "b1", "a2", "b2"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
}

func TestPausedWorkstreamKeepsItsPlaceWithoutASlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	masons(t, f, repo, map[config.WorkstreamID][]string{stream: {"a1", "a2"}, other: {"b1", "b2", "b3"}})
	var mu sync.Mutex
	pauses := []runtime.Pause{{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Source: "owner"}}
	r := &running{}
	c := f.controllerWith(t, repo, r.turns(), Options{Capacity: &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5},
		Admit: func(_ context.Context, c Candidate) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return !Held(pauses, c.Project, c), nil
		}})
	// The paused workstream sorts first by ID, yet the only slot goes to the
	// other one on every pass.
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if got, want := r.got(), []string{"b1", "b2"}; !slices.Equal(got, want) {
		t.Fatalf("ran while paused %v, want %v", got, want)
	}
	// Held turns did not advance the paused workstream's place: on resume it
	// has waited longest and takes the next slot.
	mu.Lock()
	pauses = nil
	mu.Unlock()
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if got, want := r.got(), []string{"b1", "b2", "a1", "b3", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
}

func TestProjectsKeepIndependentSlotsAndRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	limits := &config.Capacity{Masons: 5, Reviewers: 5, Committee: 5, PerWorkstream: 1}
	// Both projects have the same workstream and agent IDs.
	var repos []*trace.Repository
	var schedulers []*Scheduler
	for _, id := range []config.ProjectID{project, otherProject} {
		f, repo := setupIn(t, base, id)
		defer repo.Close()
		masons(t, f, repo, map[config.WorkstreamID][]string{stream: {"a1"}, other: {"b1"}})
		s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits})
		must(t, err)
		repos, schedulers = append(repos, repo), append(schedulers, s)
	}
	must(t, schedulers[0].Pass(ctx))
	must(t, schedulers[1].Pass(ctx))
	for i, repo := range repos {
		if got, want := dispatched(t, repo, stream, other), []string{"a1/one", "b1/one"}; !slices.Equal(got, want) {
			t.Fatalf("project %d dispatched %v, want %v", i, got, want)
		}
	}

	// The per-workstream count and the rotation are keyed by project.
	used := usage{roles: map[string]int{}, streams: map[streamKey]int{}, local: map[localKey]int{}}
	used.add(project, stream, "architect")
	s := schedulers[0]
	for _, tc := range []struct {
		project config.ProjectID
		role    string
		fits    bool
	}{{project, "mason", false}, {project, "architect", false}, {otherProject, "mason", true}, {otherProject, "architect", true}} {
		c := Candidate{Project: tc.project, Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: tc.role}}}
		if got := s.refusal(used, c) == ""; got != tc.fits {
			t.Fatalf("%s %s fits %v, want %v", tc.project, tc.role, got, tc.fits)
		}
	}
	if rotationKey(project, stream, "mason") == rotationKey(otherProject, stream, "mason") {
		t.Fatal("rotation shared across projects")
	}
}

func TestRankFollowsTheProjectsOrder(t *testing.T) {
	t.Parallel()
	const third config.WorkstreamID = "w_00000000000000000000000000000003"
	rank := Rank([]runtime.Priority{
		{Project: otherProject, Workstreams: []config.WorkstreamID{third, stream}},
		{Project: project, Workstreams: []config.WorkstreamID{other, stream}},
	}, project)
	if rank(other) != 1 || rank(stream) != 2 || rank(third) != 3 {
		t.Fatalf("ranks %d %d %d", rank(other), rank(stream), rank(third))
	}
	if rank := Rank(nil, project); rank(stream) != rank(other) {
		t.Fatal("workstreams without an order are not equal")
	}
}
