package scheduler

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const other config.WorkstreamID = "w_00000000000000000000000000000002"

// crowd creates a second workstream and mason, reviewer, committee and
// architect threads in both, each with one queued turn.
func crowd(t *testing.T) (*fixture, *trace.Repository) {
	t.Helper()
	f, repo := setup(t)
	must(t, repo.CreateWorkstream(context.Background(), other, f.clock.Now(), owner))
	for _, ws := range []config.WorkstreamID{stream, other} {
		for _, id := range []string{"mason1", "mason2", "mason3", "reviewer1", "committee1", "architect1", "architect2"} {
			f.thread(t, repo, ws, id, id[:len(id)-1])
			f.queueOn(t, repo, ws, id, "one")
		}
	}
	return f, repo
}

func counts(pairs []string) map[string]int {
	n := map[string]int{}
	for _, p := range pairs {
		agent, _, _ := strings.Cut(p, "/")
		n[strings.TrimRight(agent, "0123456789")]++
	}
	return n
}

func TestCapacityBoundsDispatch(t *testing.T) {
	ctx := context.Background()
	f, repo := crowd(t)
	defer repo.Close()
	limits := &config.Capacity{Masons: 3, Reviewers: 1, Committee: 5, PerWorkstream: 3}
	var offered []string
	admit := func(_ context.Context, c Candidate) (bool, error) {
		offered = append(offered, c.Thread.Identity.ID)
		return true, nil
	}
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits, Admit: admit})
	must(t, err)
	must(t, s.Pass(ctx))
	// Admit sees only candidates with a free slot.
	if want := []string{"architect1", "committee1", "mason1", "architect1", "committee1", "mason1"}; !slices.Equal(offered, want) {
		t.Fatalf("offered %v, want %v", offered, want)
	}
	// In workstream and agent ID order, each workstream takes one architect
	// (architects run one at a time per workstream), the committee member and
	// one mason, and then its per-workstream cap is reached.
	want := []string{"architect1/one", "committee1/one", "mason1/one"}
	if got := dispatched(t, repo, stream); !slices.Equal(got, want) {
		t.Fatalf("stream dispatched %v, want %v", got, want)
	}
	if got := dispatched(t, repo, other); !slices.Equal(got, want) {
		t.Fatalf("other dispatched %v, want %v", got, want)
	}
	// Nothing completed, so a later pass admits nothing more.
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo, stream, other); len(got) != 6 {
		t.Fatalf("second pass dispatched %v", got)
	}

	// Role limits: a wide workstream cap leaves the role kinds binding.
	f, repo = crowd(t)
	defer repo.Close()
	s, err = New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 10}})
	must(t, err)
	must(t, s.Pass(ctx))
	got := counts(dispatched(t, repo, stream, other))
	if want := map[string]int{"architect": 2, "committee": 1, "mason": 2, "reviewer": 1}; !maps.Equal(got, want) {
		t.Fatalf("dispatched by role %v, want %v", got, want)
	}
}

func TestCapacityHoldsUnderConcurrentPasses(t *testing.T) {
	f, repo := crowd(t)
	defer repo.Close()
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 3}})
	must(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Pass(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := dispatched(t, repo, stream, other)
	// stream: architect1, committee1, mason1; other: architect1, mason1 and
	// reviewer1, the committee slot being taken.
	if n := counts(got); !maps.Equal(n, map[string]int{"architect": 2, "committee": 1, "mason": 2, "reviewer": 1}) {
		t.Fatalf("dispatched %v", got)
	}
}

func TestSlotsAreReleasedWhateverTheOutcome(t *testing.T) {
	for name, reply := range map[string]func() (coreadapter.SessionResult, error){
		"success": func() (coreadapter.SessionResult, error) { return result("one").Value, nil },
		"failure": func() (coreadapter.SessionResult, error) {
			r := result("one").Value
			r.IsError = true
			return r, errors.New("backend failed")
		},
		"waiting": func() (coreadapter.SessionResult, error) {
			r := result("one").Value
			r.Outcome = &coreadapter.Outcome{Status: "waiting", Report: "Owner answer needed"}
			return r, nil
		},
		"cancellation": func() (coreadapter.SessionResult, error) {
			r := result("one").Value
			r.Cancelled = true
			return r, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, repo := setup(t, "mason1", "mason2")
			defer repo.Close()
			f.queue(t, repo, "mason1", "one")
			f.queue(t, repo, "mason2", "one")
			var ran []string
			turns := turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				ran = append(ran, strings.TrimPrefix(p.Scope.Thread, "thread_"))
				return reply()
			})
			c := f.controllerWith(t, repo, turns, Options{Capacity: &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 1}})
			must(t, c.Pass(ctx))
			if got := dispatched(t, repo); !slices.Equal(got, []string{"mason1/one"}) {
				t.Fatalf("first pass dispatched %v", got)
			}
			th, err := repo.Thread(stream, "mason1")
			must(t, err)
			if want := map[string]string{"success": "idle", "failure": "failed", "waiting": "waiting", "cancellation": "interrupted"}[name]; th.Status != want || th.Turns[0].CompletedAt.IsZero() {
				t.Fatalf("mason1 thread %+v, want status %s", th, want)
			}
			if name == "waiting" && !th.Parked() {
				t.Fatalf("mason1 thread is not parked: %+v", th)
			}
			must(t, c.Pass(ctx))
			if got := dispatched(t, repo); !slices.Equal(got, []string{"mason1/one", "mason2/one"}) {
				t.Fatalf("second pass dispatched %v", got)
			}
			if !slices.Equal(ran, []string{"mason1", "mason2"}) {
				t.Fatalf("ran %v", ran)
			}
		})
	}
}

func TestClaimsHoldSlotsUntilARestartInterruptsThem(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "mason1", "mason2", "mason3")
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}
	// A claim without an operation, as a direct runner call leaves it.
	f.queue(t, repo, "mason1", "one")
	_, err := repo.ClaimTurn(ctx, stream, "mason1", "token", t.TempDir(), f.clock.Now())
	must(t, err)
	f.queue(t, repo, "mason2", "one")
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits})
	must(t, err)
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo); len(got) != 0 {
		t.Fatalf("dispatched beside a claim: %v", got)
	}
	// After a restart the claim runs nowhere and frees its slot.
	repo = f.reopen(t, repo)
	s, err = New(repo, Options{Now: f.clock.Now, Capacity: limits})
	must(t, err)
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo); !slices.Equal(got, []string{"mason2/one"}) {
		t.Fatalf("dispatched after restart %v", got)
	}
	// A dispatched turn claimed in an earlier session frees its slot too.
	_, err = repo.ClaimTurn(ctx, stream, "mason2", "token", t.TempDir(), f.clock.Now())
	must(t, err)
	f.queue(t, repo, "mason3", "one")
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo); !slices.Equal(got, []string{"mason2/one"}) {
		t.Fatalf("dispatched beside an operation %v", got)
	}
	repo = f.reopen(t, repo)
	defer repo.Close()
	s, err = New(repo, Options{Now: f.clock.Now, Capacity: limits})
	must(t, err)
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo); !slices.Equal(got, []string{"mason2/one", "mason3/one"}) {
		t.Fatalf("dispatched after second restart %v", got)
	}
}

func TestChiefOfStaffTakesNoSlot(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "mason1", "mason2")
	defer repo.Close()
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 1}
	// The chief of staff sorts first; its turn leaves the only slot free.
	f.queueOn(t, repo, stream, trace.ChiefOfStaff, "one")
	f.queue(t, repo, "mason1", "one")
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits})
	must(t, err)
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo); !slices.Equal(got, []string{"chief_of_staff/one", "mason1/one"}) {
		t.Fatalf("dispatched %v", got)
	}
	// With every slot taken, the chief of staff's next turn still runs.
	f.queue(t, repo, "mason2", "one")
	f.queueOn(t, repo, stream, trace.ChiefOfStaff, "two")
	// A claim in this session keeps the mason slot taken.
	_, err = repo.ClaimTurn(ctx, stream, "mason1", "token", t.TempDir(), f.clock.Now())
	must(t, err)
	var ran []string
	c := f.controllerWith(t, repo, turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		ran = append(ran, strings.TrimPrefix(p.Scope.Thread, "thread_")+"/"+p.Scope.Turn)
		return result(p.Scope.Turn).Value, nil
	}), Options{Capacity: limits})
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if !slices.Equal(ran, []string{"chief_of_staff/one", "chief_of_staff/two"}) {
		t.Fatalf("ran %v", ran)
	}
	if got := dispatched(t, repo); !slices.Equal(got, []string{"chief_of_staff/one", "chief_of_staff/two", "mason1/one"}) {
		t.Fatalf("dispatched %v", got)
	}
}
