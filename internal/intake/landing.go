package intake

import (
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// featureWorkspaces returns the feature branch workspaces of the configured
// project, each workstream's on the backend the repository records for it.
func FeatureWorkspaces(cfg *config.Config, repository *trace.Repository) StreamWorkspaces {
	return StreamWorkspaces{Cfg: cfg, Repository: repository, Directory: BranchesDirectory}
}

// landingCriteria returns the criteria the unit serves, in plan order.
func LandingCriteria(unit plan.Unit) []string {
	var criteria []string
	for _, c := range unit.Criteria {
		if !slices.Contains(criteria, c) {
			criteria = append(criteria, c)
		}
	}
	return criteria
}
