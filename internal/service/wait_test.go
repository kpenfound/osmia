package service

import (
	testing "testing"
	time "time"
)

// serviceChanges subscribes before a waiter's first read. Notifications are
// hints; the timer also wakes reads of state the event stream does not expose.
func serviceChanges(t *testing.T, s *Service) (wait func(), close func()) {
	t.Helper()
	sub, ok := s.hub.Subscribe()
	if !ok {
		t.Fatal("cannot wait on a stopped service")
	}
	ticker := time.NewTicker(time.Second)
	return func() {
			t.Helper()
			select {
			case <-sub.Wake():
				sub.Take()
			case <-ticker.C:
			case <-s.hub.Done():
				t.Fatal("service stopped while waiting for state")
			}
		}, func() {
			ticker.Stop()
			s.hub.Unsubscribe(sub)
		}
}

// awaitDeferral waits for the unit's current dispatch reason.
