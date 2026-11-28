package config

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const managerProfiles = topConfig + "[profiles.manager-model]\nagent = 'codex'\nmodel = 'manager-model'\n[profiles.events]\nagent = 'codex'\nmodel = 'events-model'\n"

func managerSettings(table, capacity string) string {
	return managerProfiles + "[capacity]\n" + capacity + " = 5\n[roles." + table + "]\nprofile = 'manager-model'\nevents_profile = 'events'\nsandbox = 'sbx'\nimage = 'osmia:latest'\ncpus = 2\nmemory = '4g'\nskills = ['https://github.com/example/skills#handoff']\n"
}

func loadTop(t *testing.T, top string) (*Config, error) {
	t.Helper()
	return LoadTopLevel(fixture(t, top, projectConfig))
}

func TestManagerKeysApplyToTheManagerWithoutAWarning(t *testing.T) {
	c, err := loadTop(t, managerSettings("manager", "manager"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings for the canonical keys: %v", c.Warnings)
	}
	want := Role{Profile: "manager-model", EventsProfile: "events", Sandbox: "sbx", Image: "osmia:latest", CPUs: 2, Memory: "4g", Skills: []string{"https://github.com/example/skills#handoff"}}
	if got := c.Roles[ManagerRole]; !reflect.DeepEqual(got, want) {
		t.Fatalf("manager role %+v, want %+v", got, want)
	}
	if _, ok := c.Roles[DeprecatedManagerRole]; ok {
		t.Fatalf("the deprecated key holds settings: %+v", c.Roles)
	}
	if c.Capacity.Manager != 5 || c.Capacity.DeprecatedChief != 0 {
		t.Fatalf("capacity %+v", c.Capacity)
	}
	p, settings, err := c.Execution(ManagerRole, "manager-model")
	if err != nil || p.Model != "manager-model" || settings.Mode != "sbx" || settings.Image != "osmia:latest" || settings.CPUs != 2 || settings.Memory != "4g" || !slices.Equal(settings.Skills, want.Skills) {
		t.Fatalf("execution %+v %+v %v", p, settings, err)
	}
	if got := c.RoleExecution(ManagerRole); got.Mode != "sbx" || got.Image != "osmia:latest" {
		t.Fatalf("role execution %+v", got)
	}
}

func TestManagerDefaultsWithNeitherKey(t *testing.T) {
	c, err := loadTop(t, topConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 || c.Capacity.Manager != 3 || c.Roles[ManagerRole].Profile != "default" || c.Roles[ManagerRole].Sandbox != "none" {
		t.Fatalf("defaults: warnings %v capacity %+v role %+v", c.Warnings, c.Capacity, c.Roles[ManagerRole])
	}
}

func TestDeprecatedManagerKeysLoadIntoTheSameSettingsWithOneWarningEach(t *testing.T) {
	canonical, err := loadTop(t, managerSettings("manager", "manager"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := loadTop(t, managerSettings("chief_of_staff", "chief"))
	if err != nil {
		t.Fatalf("a config with only the deprecated keys is refused: %v", err)
	}
	if !reflect.DeepEqual(old.Roles, canonical.Roles) || old.Capacity != canonical.Capacity {
		t.Fatalf("deprecated keys load differently:\nroles %+v capacity %+v\nroles %+v capacity %+v", old.Roles, old.Capacity, canonical.Roles, canonical.Capacity)
	}
	if len(old.Warnings) != 2 {
		t.Fatalf("warnings %v, want one per deprecated key", old.Warnings)
	}
	var text []string
	for _, w := range old.Warnings {
		text = append(text, w.String())
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"roles.chief_of_staff: deprecated, use roles.manager", "capacity.chief: deprecated, use capacity.manager", "config.toml"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings lack %q:\n%s", want, joined)
		}
	}
}

func TestEachDeprecatedManagerKeyWarnsOnItsOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		top   string
		field string
	}{
		"role only":     {managerProfiles + "[roles.chief_of_staff]\nprofile = 'manager-model'\n", "roles.chief_of_staff"},
		"capacity only": {managerProfiles + "[capacity]\nchief = 2\n", "capacity.chief"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loadTop(t, tc.top)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Warnings) != 1 || c.Warnings[0].Field != tc.field {
				t.Fatalf("warnings %v, want one for %s", c.Warnings, tc.field)
			}
		})
	}
	c, err := loadTop(t, managerProfiles+"[capacity]\nchief = 2\n")
	if err != nil || c.Capacity.Manager != 2 || c.Roles[ManagerRole].Profile != "default" {
		t.Fatalf("capacity alias %+v %v", c.Capacity, err)
	}
}

func TestSettingBothFormsOfAManagerKeyIsAnErrorNamingBoth(t *testing.T) {
	for name, tc := range map[string]struct {
		top  string
		want []string
	}{
		"roles":             {managerProfiles + "[roles.manager]\nprofile = 'manager-model'\n[roles.chief_of_staff]\nprofile = 'manager-model'\n", []string{"roles.manager", "roles.chief_of_staff"}},
		"capacity":          {managerProfiles + "[capacity]\nmanager = 2\nchief = 2\n", []string{"capacity.manager", "capacity.chief"}},
		"empty role tables": {managerProfiles + "[roles.manager]\n[roles.chief_of_staff]\n", []string{"roles.manager", "roles.chief_of_staff"}},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loadTop(t, tc.top)
			var field *FieldError
			if c != nil || !errors.As(err, &field) {
				t.Fatalf("config %v, error %v", c, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

func TestDeprecatedManagerKeysAreValidatedUnderTheirOwnNames(t *testing.T) {
	for name, tc := range map[string]struct{ top, want string }{
		"alias sandbox":        {managerProfiles + "[roles.chief_of_staff]\nsandbox = 'unsafe'\n", "roles.chief_of_staff.sandbox"},
		"alias profile":        {managerProfiles + "[roles.chief_of_staff]\nprofile = 'missing'\n", "roles.chief_of_staff.profile"},
		"alias events profile": {managerProfiles + "[roles.chief_of_staff]\nevents_profile = 'missing'\n", "roles.chief_of_staff.events_profile"},
		"manager sandbox":      {managerProfiles + "[roles.manager]\nsandbox = 'unsafe'\n", "roles.manager.sandbox"},
		"manager cpus":         {managerProfiles + "[roles.manager]\ncpus = 2\n", "roles.manager.cpus"},
		"alias unknown key":    {managerProfiles + "[roles.chief_of_staff]\nbogus = 1\n", "roles.chief_of_staff.bogus"},
		"manager unknown key":  {managerProfiles + "[roles.manager]\nbogus = 1\n", "roles.manager.bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadTop(t, tc.top); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one naming %s", err, tc.want)
			}
		})
	}
}

func TestOnlyTheManagerTakesEventTurns(t *testing.T) {
	if _, err := loadTop(t, managerProfiles+"[roles.mason]\nevents_profile = 'events'\n"); err == nil || !strings.Contains(err.Error(), "roles.mason.events_profile") {
		t.Fatalf("error %v", err)
	}
}

func TestADesignerRoleKeepsItsOwnSettingsWhicheverFormTheManagerUses(t *testing.T) {
	for _, table := range []string{"manager", "chief_of_staff"} {
		c, err := loadTop(t, managerProfiles+"[roles."+table+"]\nprofile = 'manager-model'\nsandbox = 'container'\nimage = 'manager-image'\n[roles.designer]\nprofile = 'events'\n")
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Roles["designer"]; got.Profile != "events" || got.Sandbox != "none" || got.Image != "" {
			t.Fatalf("designer under [roles.%s]: %+v", table, got)
		}
		if got := c.Roles[ManagerRole]; got.Profile != "manager-model" || got.Image != "manager-image" {
			t.Fatalf("manager under [roles.%s]: %+v", table, got)
		}
	}
}
