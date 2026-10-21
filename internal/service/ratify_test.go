package service

import (
	testing "testing"

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

func (f *shedFixture) ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	return f.scenario().Ratification(t, stream, round)
}
