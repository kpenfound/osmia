package service

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"
)

// upstream gives the fixture's clone a local bare upstream, named by the
// project's upstream repository, as a second remote, and returns the commit
// its main branch is at.
func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.clone)
}

// awaitFeature waits until the workstream's feature state is the wanted one.
func (f *architectFixture) awaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	f.scenario().AwaitFeature(t, stream, want)
}

// notice returns the body of the notice the given transition told the chief
// of staff with.
func (f *architectFixture) notice(t *testing.T, stream config.WorkstreamID, transition string) string {
	t.Helper()
	return intakefixture.Notice(t, f.repository(), stream, transition)
}
