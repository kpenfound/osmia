package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func appendTopLevel(t *testing.T, root, text string) {
	t.Helper()
	file := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(file)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(file, append(data, text...), 0600))
}

func TestManagerSettingsGovernTheManagersTurnsAndCapacityUnderEitherKeyForm(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ role, capacity string }{
		"manager keys":    {"manager", "manager"},
		"deprecated keys": {"chief_of_staff", "chief"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := fixture(t)
			appendTopLevel(t, opts.Config.Root, "[capacity]\n"+tc.capacity+" = 6\n[roles."+tc.role+"]\nprofile = \"other\"\nevents_profile = \"default\"\nsandbox = \"container\"\nimage = \"manager-image\"\nskills = [\"https://github.com/acme/skills#triage\"]\n")
			s, _ := start(t, opts)
			cfg := s.current()

			profile, settings, err := s.roleExecution(cfg, trace.Manager)
			fixturekit.Must(t, err)
			if profile.Name != "other" || settings.Mode != "container" || settings.Image != "manager-image" || len(settings.Skills) != 1 {
				t.Fatalf("manager turn settings: %+v %+v", profile, settings)
			}
			if selected := cfg.RoleExecution(trace.ConfigRole(trace.Manager)); selected.Mode != "container" || selected.Image != "manager-image" {
				t.Fatalf("selected execution %+v", selected)
			}
			if _, name, err := s.managerOverride(cfg); err != nil || name.Name != "other" {
				t.Fatalf("conversation profile %+v %v", name, err)
			}
			if got := cfg.Roles[config.ManagerRole].EventsProfile; got != "default" {
				t.Fatalf("events profile %q", got)
			}
			state, _ := s.effective()
			if got := s.effectiveProfiles(state)[trace.Manager]; got.Name != "other" || got.Source != "configuration" {
				t.Fatalf("effective profile %+v", got)
			}
			for _, role := range s.capacityStatus(nil).Roles {
				if role.Role == managerRole && (role.Limit == nil || *role.Limit != 6) {
					t.Fatalf("manager capacity %+v", role)
				}
			}
			s.manager.mu.Lock()
			limit := s.manager.limit
			s.manager.mu.Unlock()
			if limit != 6 {
				t.Fatalf("manager slot pool limit %d", limit)
			}
		})
	}
}
