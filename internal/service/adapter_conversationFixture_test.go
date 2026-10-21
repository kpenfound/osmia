package service

import (
	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	testing "testing"
)

// conversationFixture creates a project with a trace holding two workstreams
// and a charter rule.
func conversationFixture(t *testing.T, prefix string) (Options, *config.Config) {
	t.Helper()
	opts, cfg := fixturekit.Conversation(t, prefix)
	o := testOptions(opts)
	o.Reconciliation.Now = (&demoClock{At: demoStart}).Now
	return o, cfg
}
