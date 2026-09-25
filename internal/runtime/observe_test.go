package runtime

import (
	"errors"
	"testing"

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
