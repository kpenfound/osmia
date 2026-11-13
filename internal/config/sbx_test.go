package config

import (
	"errors"
	"fmt"
	"testing"
)

func TestSbxRoleConfigurationAndFallback(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "opencode"} {
		for _, image := range []string{"", "example/sandbox:1"} {
			t.Run(backend+"/"+image, func(t *testing.T) {
				top := topConfig + fmt.Sprintf("fallback = 'backup'\n[profiles.backup]\nagent = %q\nmodel = 'test'\n[roles.mason]\nsandbox = 'sbx'\nimage = %q\n", backend, image)
				cfg, err := Load(fixture(t, top, projectConfig))
				if err != nil {
					t.Fatal(err)
				}
				for _, profile := range []string{"default", "backup"} {
					_, settings, err := cfg.Execution("mason", profile)
					if err != nil || settings.Mode != "sbx" || settings.Image != image {
						t.Fatalf("execution %+v: %v", settings, err)
					}
				}
			})
		}
	}
}

func TestMasonDaggerConfiguration(t *testing.T) {
	top := topConfig + "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'unix:///var/run/dagger/engine.sock'\n"
	cfg, err := Load(fixture(t, top, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	_, settings, err := cfg.Execution("mason", "default")
	if err != nil || settings.Dagger == nil || settings.Dagger.Version != "v0.20.5" || settings.Dagger.Engine != "unix:///var/run/dagger/engine.sock" {
		t.Fatalf("execution %+v: %v", settings, err)
	}
	top = topConfig + "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'docker-container://dagger-engine-v0.20.5'\n"
	if cfg, err = Load(fixture(t, top, projectConfig)); err != nil {
		t.Fatal(err)
	}
	if _, settings, err = cfg.Execution("reviewer", "default"); err != nil || settings.Dagger != nil {
		t.Fatalf("reviewer execution %+v: %v", settings, err)
	}
	for name, roles := range map[string]string{
		"other role":      "[roles.reviewer]\nsandbox = 'sbx'\n[roles.reviewer.dagger]\nversion = 'v0.20.5'\nengine = 'tcp://127.0.0.1:1234'\n",
		"not sbx":         "[roles.mason]\nsandbox = 'container'\nimage = 'example/mason:1'\n[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'tcp://127.0.0.1:1234'\n",
		"missing version": "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nengine = 'tcp://127.0.0.1:1234'\n",
		"bad version":     "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'latest'\nengine = 'tcp://127.0.0.1:1234'\n",
		"missing engine":  "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.20.5'\n",
		"bad engine":      "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'image://registry.dagger.io/engine:v0.20.5'\n",
		"unknown key":     "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'tcp://127.0.0.1:1234'\ncloud_token = 'x'\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fixture(t, topConfig+roles, projectConfig)); err == nil {
				t.Fatal("invalid Dagger configuration loaded")
			}
		})
	}
}

func TestSbxRoleSizingReachesEveryProfileOfTheRole(t *testing.T) {
	top := topConfig + "fallback = 'backup'\n[profiles.backup]\nagent = 'codex'\nmodel = 'test'\n" +
		"[roles.mason]\nsandbox = 'sbx'\nimage = 'example/sandbox:1'\ncpus = 2\nmemory = '4g'\n" +
		"[roles.reviewer]\nsandbox = 'sbx'\nmemory = '512m'\n[roles.committee]\nsandbox = 'sbx'\n"
	cfg, err := Load(fixture(t, top, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		role   string
		cpus   int
		memory string
	}{{"mason", 2, "4g"}, {"reviewer", 0, "512m"}, {"committee", 0, ""}, {"architect", 0, ""}} {
		for _, profile := range []string{"default", "backup"} {
			_, settings, err := cfg.Execution(c.role, profile)
			if err != nil || settings.CPUs != c.cpus || settings.Memory != c.memory {
				t.Fatalf("%s/%s execution %+v: %v", c.role, profile, settings, err)
			}
		}
	}
}

func TestSbxRoleSizingIsValidatedAtLoad(t *testing.T) {
	for name, c := range map[string]struct{ roles, field string }{
		"cpus without a sandbox":      {"[roles.mason]\ncpus = 2\n", "roles.mason.cpus"},
		"cpus on a host claude":       {"[roles.mason]\nsandbox = 'claude'\ncpus = 2\n", "roles.mason.cpus"},
		"cpus on a container":         {"[roles.reviewer]\nsandbox = 'container'\nimage = 'example/sandbox:1'\ncpus = 2\n", "roles.reviewer.cpus"},
		"memory without a sandbox":    {"[roles.mason]\nmemory = '4g'\n", "roles.mason.memory"},
		"memory on a host claude":     {"[roles.mason]\nsandbox = 'claude'\nmemory = '4g'\n", "roles.mason.memory"},
		"memory on a container":       {"[roles.reviewer]\nsandbox = 'container'\nimage = 'example/sandbox:1'\nmemory = '4g'\n", "roles.reviewer.memory"},
		"zero cpus":                   {"[roles.mason]\nsandbox = 'sbx'\ncpus = 0\n", "roles.mason.cpus"},
		"negative cpus":               {"[roles.mason]\nsandbox = 'sbx'\ncpus = -1\n", "roles.mason.cpus"},
		"fractional cpus":             {"[roles.mason]\nsandbox = 'sbx'\ncpus = 1.5\n", "roles.mason.cpus"},
		"cpus as text":                {"[roles.mason]\nsandbox = 'sbx'\ncpus = '2'\n", "roles.mason.cpus"},
		"memory without a unit":       {"[roles.mason]\nsandbox = 'sbx'\nmemory = '4'\n", "roles.mason.memory"},
		"memory as a number":          {"[roles.mason]\nsandbox = 'sbx'\nmemory = 4\n", "roles.mason.memory"},
		"empty memory":                {"[roles.mason]\nsandbox = 'sbx'\nmemory = ''\n", "roles.mason.memory"},
		"zero memory":                 {"[roles.mason]\nsandbox = 'sbx'\nmemory = '0g'\n", "roles.mason.memory"},
		"negative memory":             {"[roles.mason]\nsandbox = 'sbx'\nmemory = '-4g'\n", "roles.mason.memory"},
		"fractional memory":           {"[roles.mason]\nsandbox = 'sbx'\nmemory = '1.5g'\n", "roles.mason.memory"},
		"upper case unit":             {"[roles.mason]\nsandbox = 'sbx'\nmemory = '4G'\n", "roles.mason.memory"},
		"other unit":                  {"[roles.mason]\nsandbox = 'sbx'\nmemory = '4gb'\n", "roles.mason.memory"},
		"space before the unit":       {"[roles.mason]\nsandbox = 'sbx'\nmemory = '4 g'\n", "roles.mason.memory"},
		"option in the value":         {"[roles.mason]\nsandbox = 'sbx'\nmemory = '4g --privileged'\n", "roles.mason.memory"},
		"shell metacharacters":        {"[roles.mason]\nsandbox = 'sbx'\nmemory = '4g;reboot'\n", "roles.mason.memory"},
		"sizing key not on knownKey":  {"[roles.mason]\nsandbox = 'sbx'\ncpu = 2\n", "roles.mason.cpu"},
		"sizing in the dagger table":  {"[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'tcp://127.0.0.1:1234'\ncpus = 2\n", "roles.mason.dagger.cpus"},
		"another role, wrong sandbox": {"[roles.committee]\nsandbox = 'none'\nmemory = '2g'\n", "roles.committee.memory"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(fixture(t, topConfig+c.roles, projectConfig))
			var field *FieldError
			if !errors.As(err, &field) || field.Field != c.field {
				t.Fatalf("got %#v (%v), want an error naming %s", field, err, c.field)
			}
		})
	}
	for _, roles := range []string{
		"[roles.mason]\nsandbox = 'sbx'\ncpus = 1\nmemory = '512m'\n",
		"[roles.reviewer]\nsandbox = 'sbx'\nmemory = '32g'\n",
		"[roles.mason]\nsandbox = 'sbx'\ncpus = 64\n",
	} {
		if _, err := Load(fixture(t, topConfig+roles, projectConfig)); err != nil {
			t.Fatalf("%q: %v", roles, err)
		}
	}
}
