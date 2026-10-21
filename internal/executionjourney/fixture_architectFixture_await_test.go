package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	testing "testing"
)

// await polls the workstream's feature and draft states until want accepts
// them.
func (f *architectFixture) await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	f.scenario().Await(t, stream, want)
}
