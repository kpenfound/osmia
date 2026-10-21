package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// lifecycleAwait observes the live controller's durable writes. It never runs a
// second controller or a reconciliation pass from the test.
func lifecycleAwait(t *testing.T, f *productionDesignFixture, phase string, ready func() bool) {
	t.Helper()
	f.sharedDesign().LifecycleAwait(t, phase, ready)
}

func lifecycleWorker(t *testing.T, f *productionDesignFixture, project config.ProjectID, stream config.WorkstreamID, worker, turn, prompt string) {
	t.Helper()
	f.sharedDesign().LifecycleWorker(t, project, stream, worker, turn, prompt)
}
