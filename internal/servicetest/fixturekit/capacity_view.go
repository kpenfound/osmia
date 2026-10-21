package fixturekit

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

func RoleCapacityEntry(t *testing.T, c *api.CapacityStatus, role string) api.RoleCapacity {
	t.Helper()
	for _, r := range c.Roles {
		if r.Role == role {
			return r
		}
	}
	t.Fatalf("capacity has no %s: %+v", role, c)
	return api.RoleCapacity{}
}
