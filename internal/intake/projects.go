package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var (
	RegistrationActor = trace.Actor{Kind: "owner", ID: "local"}
	ServiceActor      = trace.Actor{Kind: "service", ID: "osmia"}
)

// groupMembers returns every repo of the project default belongs to, default
// itself included, in the order they appear in all.
func GroupMembers(all []config.Project, group config.ProjectID) []config.Project {
	var members []config.Project
	for _, p := range all {
		if p.Group() == group {
			members = append(members, p)
		}
	}
	return members
}
