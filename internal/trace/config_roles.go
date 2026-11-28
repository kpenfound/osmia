package trace

import (
	"slices"

	"github.com/kpenfound/osmia/internal/config"
)

// ConfigRole is the configuration key that holds the settings of a runtime
// role. The manager's turns run under its stored role name, and its settings
// sit under roles.manager; every other role's settings sit under its own name.
func ConfigRole(role string) string {
	if role == ChiefOfStaff {
		return config.ManagerRole
	}
	return role
}

// RuntimeRole is the runtime role whose settings sit under a configuration
// role key. It inverts ConfigRole.
func RuntimeRole(key string) string {
	if key == config.ManagerRole {
		return ChiefOfStaff
	}
	return key
}

// ConfiguredRoles lists the runtime roles that cfg holds settings for, sorted
// by name.
func ConfiguredRoles(cfg *config.Config) []string {
	roles := make([]string, 0, len(cfg.Roles))
	for key := range cfg.Roles {
		roles = append(roles, RuntimeRole(key))
	}
	slices.Sort(roles)
	return roles
}
