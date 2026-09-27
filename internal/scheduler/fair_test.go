package scheduler

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const third config.WorkstreamID = "w_00000000000000000000000000000003"

// contention is two projects under one root drawing on one pool with one
// mason slot. The first project has three workstreams ready, two mason turns
// in each: s1 and s2 in stream, o1 and o2 in other, t1 and t2 in third. The
// second project has one workstream with three: z1, z2 and z3. Both
// projects read one clock.
type contention struct {
	repos  [2]*trace.Repository
	shared *Shared
	clock  *clock
	turns  *running
}

func newContention(t *testing.T) *contention {
	t.Helper()
	base, ctx := t.TempDir(), context.Background()
	c := &contention{clock: &clock{now: start}, turns: &running{}}
	c.shared = &Shared{Traces: func() []*trace.Repository { return c.repos[:] }}
	for i, id := range []config.ProjectID{project, second} {
		f, repo := setupIn(t, base, id)
		t.Cleanup(func() { repo.Close() })
		f.clock = c.clock
		threads := map[config.WorkstreamID][]string{stream: {"z1", "z2", "z3"}}
		if i == 0 {
			threads = map[config.WorkstreamID][]string{stream: {"s1", "s2"}, other: {"o1", "o2"}, third: {"t1", "t2"}}
			must(t, repo.CreateWorkstream(ctx, other, f.clock.Now(), owner))
			must(t, repo.CreateWorkstream(ctx, third, f.clock.Now(), owner))
		}
		for _, ws := range []config.WorkstreamID{stream, other, third} {
			for _, agent := range threads[ws] {
				f.thread(t, repo, ws, agent, "mason")
				f.queueOn(t, repo, ws, agent, "one")
			}
		}
		c.repos[i] = repo
	}
	return c
}

// controllers returns a controller per project whose scheduler draws on the
// pool with the given options, each running its turns to completion.
func (c *contention) controllers(t *testing.T, options Options) [2]interface{ Pass(context.Context) error } {
	t.Helper()
	var out [2]interface{ Pass(context.Context) error }
	for i, repo := range c.repos {
		f := &fixture{clock: c.clock}
		o := options
		o.Shared = c.shared
		if o.Capacity == nil {
			o.Capacity = &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}
		}
		out[i] = f.controllerWith(t, repo, c.turns.turns(), o)
	}
	return out
}

// At equal priority the one slot alternates between the projects, although
// the first project has three workstreams ready and the second one, and
// within the first project it rotates across its workstreams.
func TestEqualPriorityProjectsTakeSlotsInTurn(t *testing.T) {
	ctx := context.Background()
	c := newContention(t)
	loops := c.controllers(t, Options{})
	must(t, loops[0].Pass(ctx))
	// Another pass of the first project leaves the freed slot to the second
	// project, whose turn now goes first.
	must(t, loops[0].Pass(ctx))
	if got := c.turns.got(); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("ran %v before the second project passed; want s1 alone", got)
	}
	for range 6 {
		must(t, loops[1].Pass(ctx))
		must(t, loops[0].Pass(ctx))
	}
	if got, want := c.turns.got(), []string{"s1", "z1", "o1", "z2", "t1", "z3", "s2", "o2", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
}

// A workstream named in the priority order goes before the rotation: while
// it has a turn ready, the other project's pass leaves the slot to it.
func TestExplicitPriorityBeatsTheRotationAcrossProjects(t *testing.T) {
	ctx := context.Background()
	c := newContention(t)
	loops := c.controllers(t, Options{Priorities: func() []runtime.Ranked { return []runtime.Ranked{{Project: project, Workstream: other}} }})
	for range 9 {
		must(t, loops[0].Pass(ctx))
		must(t, loops[1].Pass(ctx))
	}
	if got, want := c.turns.got(), []string{"o1", "o2", "z1", "s1", "z2", "t1", "z3", "s2", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}

	// The same holds for a workstream of the second project.
	c = newContention(t)
	loops = c.controllers(t, Options{Priorities: func() []runtime.Ranked { return []runtime.Ranked{{Project: second, Workstream: stream}} }})
	for range 9 {
		must(t, loops[0].Pass(ctx))
		must(t, loops[1].Pass(ctx))
	}
	if got, want := c.turns.got(), []string{"z1", "z2", "z3", "s1", "o1", "t1", "s2", "o2", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
}

// A turn the other project's own gate declined holds no slot: once that
// project's pass declined it, this project's pass takes the slot, until the
// other project's gate admits it again.
func TestAnotherProjectsDeclinedTurnHoldsNoSlot(t *testing.T) {
	ctx := context.Background()
	c := newContention(t)
	var mu sync.Mutex
	paused := true
	loops := c.controllers(t, Options{Admit: func(_ context.Context, cand Candidate) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return cand.Project != second || !paused, nil
	}})
	must(t, loops[0].Pass(ctx))
	// The second project has not passed yet, so its turn goes first.
	must(t, loops[0].Pass(ctx))
	must(t, loops[1].Pass(ctx))
	must(t, loops[0].Pass(ctx))
	must(t, loops[0].Pass(ctx))
	if got, want := c.turns.got(), []string{"s1", "o1", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	mu.Lock()
	paused = false
	mu.Unlock()
	must(t, loops[1].Pass(ctx))
	must(t, loops[0].Pass(ctx))
	if got, want := c.turns.got(), []string{"s1", "o1", "t1", "z1", "s2"}; !slices.Equal(got, want) {
		t.Fatalf("after the second project's gate admits its turns: ran %v, want %v", got, want)
	}
}

// A pass holds another project's turn to that project's per-workstream
// limit, as the other project's latest pass used it, and takes the slot a
// turn beyond it cannot.
func TestAnotherProjectsWorkstreamCapHoldsInThePass(t *testing.T) {
	ctx := context.Background()
	c := newContention(t)
	var schedulers [2]*Scheduler
	for i, perWorkstream := range []int{5, 1} {
		s, err := New(c.repos[i], Options{Now: c.clock.Now, Shared: c.shared, Capacity: &config.Capacity{Masons: 3, Reviewers: 1, Committee: 1, PerWorkstream: perWorkstream}})
		must(t, err)
		schedulers[i] = s
	}
	// The second project's pass dispatches z1 and leaves two slots to the
	// first project; z2 is beyond its workstream's limit.
	must(t, schedulers[1].Pass(ctx))
	if got := dispatched(t, c.repos[1]); !slices.Equal(got, []string{"z1/one"}) {
		t.Fatalf("second project dispatched %v", got)
	}
	must(t, schedulers[0].Pass(ctx))
	if got := dispatched(t, c.repos[0], stream, other, third); len(got) != 2 {
		t.Fatalf("first project dispatched %v; want two turns beside z1", got)
	}
}
