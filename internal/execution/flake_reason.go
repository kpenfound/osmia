package execution

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/flakes"
)

// KnownChecks are the check links a run shows the project to have: the links
// it selected, the checks that ran together in its invocation and those that
// failed or were flaky. They tie a Dagger Cloud check name to the check it
// stands for.
func (r UnitCheckRun) KnownChecks() []string {
	items := slices.Concat(r.Failed, r.Flaky)
	if r.Selection != nil {
		items = slices.Concat(r.Selection.Links, items)
	}
	if r.Evidence != nil {
		items = slices.Concat(items, r.Evidence.CoRun)
	}
	return flakes.CheckLinks(items...)
}

// openFlakeFixes names the flake-fix workstreams open for the groups of the
// run's flaky checks, for the reason a unit's pass records. It is a read of
// the flake record and answers nothing when the project has the setting off,
// the run has no flaky check, or the record cannot be read.
func (c *checkers) openFlakeFixes(run UnitCheckRun) string {
	if !c.cfg.Project.FlakeFixes || len(run.Flaky) == 0 {
		return ""
	}
	store := &flakes.Store{Repository: c.repository}
	var named []string
	for _, g := range flakes.GroupFlaky(run.KnownChecks(), run.Flaky) {
		workstream, open, err := store.OpenWorkstream(g.Group)
		if err != nil {
			return ""
		}
		if open {
			named = append(named, fmt.Sprintf("flake-fix workstream %s is open for flake group %s", workstream, g.Group))
		}
	}
	return strings.Join(named, "; ")
}
