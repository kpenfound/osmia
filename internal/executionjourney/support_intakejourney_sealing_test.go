package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.clone)
}

func (f *architectFixture) awaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	f.scenario().AwaitFeature(t, stream, want)
}

func (f *architectFixture) notice(t *testing.T, stream config.WorkstreamID, transition string) string {
	t.Helper()
	return intakefixture.Notice(t, f.repository(), stream, transition)
}
