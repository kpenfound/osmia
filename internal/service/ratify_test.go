package service

import (
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	shed "github.com/kpenfound/osmia/internal/shed"
)

// inboxEntries returns the inbox entries of one kind and workstream.
func (f *shedFixture) inboxEntries(t *testing.T, stream config.WorkstreamID, kind string) []InboxEntry {
	t.Helper()
	return f.scenario().InboxEntries(t, stream, kind)
}

// raisedContests waits for the chief of staff to leave a contest of the
// workstream to the owner, and returns the inbox's contested entries.
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

func (f *shedFixture) ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	return f.scenario().Ratification(t, stream, round)
}
