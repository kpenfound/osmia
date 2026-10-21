package fixturekit

import (
	"testing"
	"time"
)

// ConversationAwait waits for the conversation path to settle.
func ConversationAwait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(DemoTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("conversation path did not settle")
		case <-tick.C:
		}
	}
}
