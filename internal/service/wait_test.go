package service

import (
	"testing"
	"time"
)

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
