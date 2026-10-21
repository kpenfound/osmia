package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	testing "testing"
	time "time"
)

func (f *shedFixture) raisedContests(t *testing.T, stream config.WorkstreamID) []InboxEntry {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		if entries := f.inboxEntries(t, stream, InboxContested); len(entries) != 0 || time.Now().After(deadline) {
			return entries
		}
		time.Sleep(20 * time.Millisecond)
	}
}
