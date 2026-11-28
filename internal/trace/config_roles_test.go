package trace

import (
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestTheManagersRuntimeRoleMapsToItsConfigurationKeyAndBack(t *testing.T) {
	if got := ConfigRole(Manager); got != config.ManagerRole {
		t.Fatalf("config key of the runtime role %q: %q", Manager, got)
	}
	if got := RuntimeRole(config.ManagerRole); got != Manager {
		t.Fatalf("runtime role of the config key: %q", got)
	}
	for _, role := range []string{"mason", "reviewer", "architect", "committee", "designer", "foreman"} {
		if ConfigRole(role) != role || RuntimeRole(role) != role {
			t.Fatalf("role %s does not map to itself", role)
		}
	}
}

func TestConfiguredRolesNameTheManagerByItsRuntimeRole(t *testing.T) {
	cfg := &config.Config{Roles: map[string]config.Role{"mason": {}, config.ManagerRole: {}, "architect": {}}}
	if got := ConfiguredRoles(cfg); !slices.Equal(got, []string{"architect", Manager, "mason"}) {
		t.Fatalf("roles %v", got)
	}
}
