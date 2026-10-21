package service

import (
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	testing "testing"
)

// projectFixture writes a root without a project and a local Git clone.
func projectFixture(t *testing.T) (Options, string) {
	t.Helper()
	cfg, clone := fixturekit.ProjectConfig(t)
	o := testOptions(cfg)
	o.Workstreams = nil
	o.reviewChecks = passingChecks
	return o, clone
}
