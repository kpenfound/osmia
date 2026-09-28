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
