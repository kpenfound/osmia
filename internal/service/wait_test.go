package service

import "testing"

func serviceChanges(t *testing.T, s *Service) (wait func(), close func()) {
	t.Helper()
	changed, release, ok := InspectForTest(s).Changes()
	if !ok {
		t.Fatal("cannot wait on a stopped service")
	}
	return func() {
		t.Helper()
		if changed() != nil {
			t.Fatal("service stopped while waiting for state")
		}
	}, release
}
