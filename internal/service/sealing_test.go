package service

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

// upstream gives the fixture's clone a local bare upstream, named by the
// project's upstream repository, as a second remote, and returns the commit
// its main branch is at.
func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.clone)
}
