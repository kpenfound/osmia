package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	testing "testing"
	time "time"
)

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
