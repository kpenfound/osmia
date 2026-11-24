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
				top := topConfig + fmt.Sprintf("fallback = 'backup'\n[profiles.backup]\nagent = %q\nmodel = 'test'\n[roles.mason]\nsandbox = 'sbx'\nimage = %q\n", backend, image) + managedDagger
				cfg, err := load(fixture(t, top, projectConfig))
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

const managedDagger = "[dagger]\nengine = 'managed'\nversion = 'v0.20.5'\n"

// A mason in sbx runs Dagger on the service's engine: a host engine with the
// top-level release, or Dagger Cloud with the top-level token variable. Other
// roles and masons outside sbx get no Dagger.
func TestMasonDaggerFollowsTheServiceEngine(t *testing.T) {
	for _, tc := range []struct {
		name, dagger, engine, token string
	}{
		{"managed", managedDagger, "docker-container://osmia-dagger-engine", ""},
		{"socket", "[dagger]\nengine = 'unix:///var/run/dagger/engine.sock'\nversion = 'v0.20.5'\n", "unix:///var/run/dagger/engine.sock", ""},
		{"container", "[dagger]\nengine = 'docker-container://dagger-engine-v0.20.5'\nversion = 'v0.20.5'\n", "docker-container://dagger-engine-v0.20.5", ""},
		{"cloud", "[dagger]\nengine = 'cloud'\ncloud_compute_token_env = 'OSMIA_CLOUD_TOKEN'\n", "", "OSMIA_CLOUD_TOKEN"},
		{"managed with cloud fallback", "[dagger]\nengine = 'managed'\nversion = 'v0.20.5'\ncloud_compute_token_env = 'OSMIA_CLOUD_TOKEN'\ncheck_fallback = 'cloud'\n", "docker-container://osmia-dagger-engine", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(fixture(t, topConfig+"[roles.mason]\nsandbox = 'sbx'\n"+tc.dagger, projectConfig))
			if err != nil {
				t.Fatal(err)
			}
			for _, profile := range []string{"default"} {
				_, settings, err := cfg.Execution("mason", profile)
				if err != nil || settings.DaggerCloudTokenEnv != tc.token {
					t.Fatalf("execution %+v: %v", settings, err)
				}
				if tc.engine == "" && settings.Dagger != nil {
					t.Fatalf("a Dagger Cloud mason was given a host engine: %+v", settings.Dagger)
				}
				if tc.engine != "" && (settings.Dagger == nil || settings.Dagger.Engine != tc.engine || settings.Dagger.Version != "v0.20.5") {
					t.Fatalf("mason Dagger %+v, want %s at v0.20.5", settings.Dagger, tc.engine)
				}
			}
			for _, role := range roleNames {
				if role == "mason" {
					continue
				}
				if _, settings, err := cfg.Execution(role, "default"); err != nil || settings.RunsDagger() {
					t.Fatalf("%s execution %+v: %v", role, settings, err)
				}
			}
		})
	}
	for sandbox, roles := range map[string]string{
		"container": "[roles.mason]\nsandbox = 'container'\nimage = 'example/mason:1'\n",
		"none":      "[roles.mason]\nsandbox = 'none'\n",
	} {
		cfg, err := load(fixture(t, topConfig+roles, projectConfig))
		if err != nil {
			t.Fatalf("%s: %v", sandbox, err)
		}
		if _, settings, err := cfg.Execution("mason", "default"); err != nil || settings.RunsDagger() {
			t.Fatalf("%s mason execution %+v: %v", sandbox, settings, err)
		}
	}
}

// A mason in sbx refuses a service engine it cannot follow: auto, which a
// sandbox has no address for, and a host engine without the CLI release to
// install.
func TestMasonDaggerRefusesAnEngineItCannotFollow(t *testing.T) {
	for name, c := range map[string]struct{ dagger, field string }{
		"no dagger section":   {"", "dagger.engine"},
		"auto":                {"[dagger]\nengine = 'auto'\nversion = 'v0.20.5'\n", "dagger.engine"},
		"managed, no version": {"[dagger]\nengine = 'managed'\n", "dagger.version"},
		"address, no version": {"[dagger]\nengine = 'tcp://127.0.0.1:1234'\n", "dagger.version"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(fixture(t, topConfig+"[roles.mason]\nsandbox = 'sbx'\n"+c.dagger, projectConfig))
			var field *FieldError
			if !errors.As(err, &field) || field.Field != c.field {
				t.Fatalf("got %#v (%v), want an error naming %s", field, err, c.field)
			}
		})
	}
}

func TestSbxRoleSizingReachesEveryProfileOfTheRole(t *testing.T) {
	top := topConfig + "fallback = 'backup'\n[profiles.backup]\nagent = 'codex'\nmodel = 'test'\n" +
		"[roles.mason]\nsandbox = 'sbx'\nimage = 'example/sandbox:1'\ncpus = 2\nmemory = '4g'\n" +
		"[roles.reviewer]\nsandbox = 'sbx'\nmemory = '512m'\n[roles.committee]\nsandbox = 'sbx'\n" + managedDagger
	cfg, err := load(fixture(t, top, projectConfig))
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
		"another role, wrong sandbox": {"[roles.committee]\nsandbox = 'none'\nmemory = '2g'\n", "roles.committee.memory"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(fixture(t, topConfig+c.roles, projectConfig))
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
		if _, err := load(fixture(t, topConfig+roles+managedDagger, projectConfig)); err != nil {
			t.Fatalf("%q: %v", roles, err)
		}
	}
}
