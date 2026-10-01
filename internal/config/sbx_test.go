package config

import (
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
