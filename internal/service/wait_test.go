package service

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// serviceChanges subscribes before a waiter's first read. Notifications are
// hints; the timer also wakes reads of state the event stream does not expose.
func serviceChanges(t *testing.T, s *Service) (wait func(), close func()) {
	t.Helper()
	sub, ok := s.hub.subscribe()
	if !ok {
		t.Fatal("cannot wait on a stopped service")
	}
	ticker := time.NewTicker(time.Second)
	return func() {
			t.Helper()
			select {
			case <-sub.wake:
				sub.take()
			case <-ticker.C:
			case <-s.hub.done:
				t.Fatal("service stopped while waiting for state")
			}
		}, func() {
			ticker.Stop()
			s.hub.unsubscribe(sub)
		}
}

// awaitDeferral waits for the unit's current dispatch reason.
func (f *shedFixture) awaitDeferral(t *testing.T, stream config.WorkstreamID, unit, reason string) {
	t.Helper()
	wait, close := serviceChanges(t, f.s)
	defer close()
	deadline := time.Now().Add(demoTimeout)
	want := DispatchDeferred + " " + reason
	for {
		decisions := f.dispatches(t, stream, unit)
		if len(decisions) > 0 && decisions[len(decisions)-1] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit %s dispatches %q, want %q", unit, decisions, want)
		}
		wait()
	}
}
