package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// The observer hears every mutation that changes the persisted state, and
// neither a mutation that leaves it as it was nor a refused one.
func TestObserverHearsChangingMutations(t *testing.T) {
	t.Parallel()
	s := open(t, fixture(t))
	heard := 0
	s.Observe(func() { heard++ })
	must(t, s.SetProfile("mason", "other"))
	if heard != 1 {
		t.Fatalf("after an override: %d", heard)
	}
	must(t, s.SetProfile("mason", "other"))
	if heard != 1 {
		t.Fatalf("after repeating the override: %d", heard)
	}
	if err := s.SetProfile("mason", "missing"); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown profile: %v", err)
	}
	must(t, s.SetPriority(Priority{Project: pid, Workstreams: []config.WorkstreamID{w1}}))
	must(t, s.ClearProfile("mason"))
	if heard != 3 {
		t.Fatalf("after a refused override, a priority and a clear: %d", heard)
	}
}

// The pause observer hears each stored pause a mutation removes, a resume or a
// replacement on the same target, and nothing for a pause that is only
// rewritten as it was.
func TestPauseEndObserverHearsRemovedPauses(t *testing.T) {
	t.Parallel()
	s := open(t, fixture(t))
	type ended struct {
		pause    Pause
		replaced bool
	}
	var heard []ended
	s.ObservePauseEnd(func(p Pause, replaced bool) { heard = append(heard, ended{p, replaced}) })
	target := Target{Scope: "workstream", Project: pid, Workstream: w1}
	set := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	first := Pause{Target: target, Mode: "soft", Reason: "first", Source: PauseOwner, SetAt: set}
	must(t, s.SetPause(first))
	must(t, s.SetPause(first))
	if len(heard) != 0 {
		t.Fatalf("a pause that is set heard as ended: %+v", heard)
	}
	second := Pause{Target: target, Mode: "hard", Reason: "second", Source: PauseOwner, SetAt: set.Add(time.Minute)}
	must(t, s.SetPause(second))
	if len(heard) != 1 || heard[0].pause != first || !heard[0].replaced {
		t.Fatalf("after replacing the pause: %+v", heard)
	}
	must(t, s.ClearPause(target, PauseOwner))
	if len(heard) != 2 || heard[1].pause != second || heard[1].replaced {
		t.Fatalf("after resuming: %+v", heard)
	}
	must(t, s.ClearPause(target, PauseOwner))
	if len(heard) != 2 {
		t.Fatalf("after resuming a target that is not paused: %+v", heard)
	}
}
