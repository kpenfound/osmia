package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	testing "testing"
)

func (f *shedFixture) inboxEntries(t *testing.T, stream config.WorkstreamID, kind string) []InboxEntry {
	t.Helper()
	return f.scenario().InboxEntries(t, stream, kind)
}
