package config

import (
	"slices"
	"strings"
	"testing"
)

func TestRoleSkills(t *testing.T) {
	refs := []string{"https://github.com/acme/skills#skills/tdd", "git@github.com:acme/review.git@v1"}
	c, err := Load(fixture(t, topConfig+"[skills]\nrefresh = 'always'\n[roles.reviewer]\nskills = ['"+strings.Join(refs, "', '")+"']\n", projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Skills.Refresh != "always" || !slices.Equal(c.Roles["reviewer"].Skills, refs) {
		t.Fatalf("skills %+v reviewer %+v", c.Skills, c.Roles["reviewer"])
	}
	if _, settings, err := c.Execution("reviewer", "default"); err != nil || !slices.Equal(settings.Skills, refs) {
		t.Fatalf("reviewer settings %+v: %v", settings, err)
	}
	if _, settings, err := c.Execution("mason", "default"); err != nil || len(settings.Skills) != 0 {
		t.Fatalf("mason settings %+v: %v", settings, err)
	}
	defaults, err := Load(fixture(t, topConfig, projectConfig))
	if err != nil || defaults.Skills.Refresh != "24h" {
		t.Fatalf("default refresh %+v: %v", defaults.Skills, err)
	}
}

func TestInvalidRoleSkills(t *testing.T) {
	for name, c := range map[string]struct{ top, want string }{
		"empty reference":   {"[roles.mason]\nskills = ['']\n", "roles.mason.skills[0]"},
		"leaves repository": {"[roles.mason]\nskills = ['https://x/ok', 'https://x/s#../up']\n", "roles.mason.skills[1]"},
		"same name":         {"[roles.architect]\nskills = ['https://x/skills', 'https://y/skills.git']\n", "roles.architect.skills[1]: another skill of the role has the name skills"},
		"duplicate":         {"[roles.architect]\nskills = ['https://x/skills', 'https://x/skills']\n", "roles.architect.skills[1]"},
		"not a list":        {"[roles.mason]\nskills = 'https://x/skills'\n", "roles.mason.skills"},
		"refresh":           {"[skills]\nrefresh = 'daily'\n", "skills.refresh"},
		"negative refresh":  {"[skills]\nrefresh = '-1h'\n", "skills.refresh"},
		"unknown key":       {"[skills]\ncache = '/tmp'\n", "skills.cache"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(fixture(t, topConfig+c.top, projectConfig))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}
