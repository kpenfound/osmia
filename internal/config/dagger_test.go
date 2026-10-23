package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestServiceDaggerConfiguration(t *testing.T) {
	// Config loading must work without a CLI or engine and never read the token.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("OSMIA_CHECK_TOKEN", "private-cloud-token")
	for _, tc := range []struct {
		name, section string
		want          ResolvedDagger
	}{
		{"absent", "", ResolvedDagger{Mode: DaggerEngineAuto}},
		{"empty section", "[dagger]\n", ResolvedDagger{Mode: DaggerEngineAuto}},
		{"default engine", "[dagger]\nversion = 'v0.20.5'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\n", ResolvedDagger{Mode: DaggerEngineAuto, Version: "v0.20.5", CloudTokenEnv: "OSMIA_CHECK_TOKEN"}},
		{"auto", "[dagger]\nengine = 'auto'\n", ResolvedDagger{Mode: DaggerEngineAuto}},
		{"managed", "[dagger]\nengine = 'managed'\nversion = '0.20.5'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\n", ResolvedDagger{Mode: DaggerEngineManaged, Address: "docker-container://osmia-dagger-engine", Version: "0.20.5", CloudTokenEnv: "OSMIA_CHECK_TOKEN"}},
		{"cloud", "[dagger]\nengine = 'cloud'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\n", ResolvedDagger{Mode: DaggerEngineCloud, CloudTokenEnv: "OSMIA_CHECK_TOKEN"}},
		{"unix", "[dagger]\nengine = 'unix:///run/dagger.sock'\n", ResolvedDagger{Mode: DaggerEngineAddress, Address: "unix:///run/dagger.sock"}},
		{"tcp", "[dagger]\nengine = 'tcp://127.0.0.1:1234'\n", ResolvedDagger{Mode: DaggerEngineAddress, Address: "tcp://127.0.0.1:1234"}},
		{"container", "[dagger]\nengine = 'docker-container://custom-engine'\n", ResolvedDagger{Mode: DaggerEngineAddress, Address: "docker-container://custom-engine"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(fixture(t, topConfig+tc.section, projectConfig))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Dagger.Resolve(); got != tc.want {
				t.Fatalf("resolved engine: %+v, want %+v", got, tc.want)
			}
			if tc.want.Mode == DaggerEngineAuto && cfg.Dagger.Engine != "auto" {
				t.Fatalf("default engine: %q", cfg.Dagger.Engine)
			}
			_, settings, err := cfg.Execution("mason", "default")
			if err != nil || settings.Dagger != nil {
				t.Fatalf("mason without a Dagger section: %+v, %v", settings, err)
			}
			data, err := json.Marshal(cfg)
			if err != nil || strings.Contains(string(data), "private-cloud-token") {
				t.Fatalf("config must not read the Cloud credential: %v", err)
			}
		})
	}
	if got := (DaggerConfig{}).Resolve(); got != (ResolvedDagger{Mode: DaggerEngineAuto}) {
		t.Fatalf("zero-value selection: %+v", got)
	}
}

func TestServiceDaggerValidation(t *testing.T) {
	const secret = "private-secret"
	for _, tc := range []struct{ name, section, field string }{
		{"invalid mode", "engine = 'private-secret'", "dagger.engine"},
		{"empty engine", "engine = ''", "dagger.engine"},
		{"bad socket", "engine = 'unix://private-secret'", "dagger.engine"},
		{"bad port", "engine = 'tcp://private-secret:99999'", "dagger.engine"},
		{"bad container", "engine = 'docker-container://private-secret/path'", "dagger.engine"},
		{"unsupported scheme", "engine = 'https://private-secret@example.com'", "dagger.engine"},
		{"bad version", "version = 'private-secret'", "dagger.version"},
		{"empty version", "version = ''", "dagger.version"},
		{"bad env name", "cloud_token_env = 'private-secret'", "dagger.cloud_token_env"},
		{"cloud without env", "engine = 'cloud'", "dagger.cloud_token_env"},
		{"cloud empty env", "engine = 'cloud'\ncloud_token_env = ''", "dagger.cloud_token_env"},
		{"unknown key", "cloud_token = 'private-secret'", "dagger.cloud_token"},
		{"nested key", "[dagger.extra]\nvalue = 'private-secret'", "dagger.extra"},
		{"wrong engine type", "engine = ['private-secret']", "dagger.engine"},
		{"wrong version type", "version = ['private-secret']", "dagger.version"},
		{"wrong env type", "cloud_token_env = ['private-secret']", "dagger.cloud_token_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadTopLevel(fixture(t, topConfig+"[dagger]\n"+tc.section+"\n", projectConfig))
			var field *FieldError
			if cfg != nil || !errors.As(err, &field) || field.Field != tc.field {
				t.Fatalf("load: %+v, %v; want field %s", cfg, err, tc.field)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("field error exposed the configured value")
			}
		})
	}
}

func TestMasonDaggerEngineDefault(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name, service, mason, want string
	}{
		{"legacy explicit", "", "engine = 'unix:///run/mason.sock'\n", "unix:///run/mason.sock"},
		{"managed default", "[dagger]\nengine = 'managed'\n", "", "docker-container://osmia-dagger-engine"},
		{"address default", "[dagger]\nengine = 'tcp://127.0.0.1:1234'\n", "", "tcp://127.0.0.1:1234"},
		{"unix default", "[dagger]\nengine = 'unix:///run/service.sock'\n", "", "unix:///run/service.sock"},
		{"container default", "[dagger]\nengine = 'docker-container://custom'\n", "", "docker-container://custom"},
		{"managed override", "[dagger]\nengine = 'managed'\n", "engine = 'unix:///run/mason.sock'\n", "unix:///run/mason.sock"},
		{"cloud override", "[dagger]\nengine = 'cloud'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\n", "engine = 'unix:///run/mason.sock'\n", "unix:///run/mason.sock"},
		{"auto missing", "[dagger]\nengine = 'auto'\n", "", ""},
		{"legacy missing", "", "", ""},
		{"cloud missing", "[dagger]\nengine = 'cloud'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\n", "", ""},
		{"cloud mason", "[dagger]\nengine = 'managed'\n", "engine = 'cloud'\n", ""},
		{"empty mason", "[dagger]\nengine = 'managed'\n", "engine = ''\n", ""},
		{"bad mason", "[dagger]\nengine = 'managed'\n", "engine = 'private-secret'\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			top := topConfig + tc.service + "[roles.mason]\nsandbox = 'sbx'\nimage = 'example/mason:1'\n[roles.mason.dagger]\nversion = 'v0.20.5'\n" + tc.mason
			cfg, err := Load(fixture(t, top, projectConfig))
			if tc.want == "" {
				var field *FieldError
				if cfg != nil || !errors.As(err, &field) || field.Field != "roles.mason.dagger.engine" {
					t.Fatalf("expected mason engine field error, got %+v, %v", cfg, err)
				}
				if strings.Contains(err.Error(), "private-secret") {
					t.Fatal("field error exposed the mason engine value")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, settings, err := cfg.Execution("mason", "default")
			if err != nil || settings.Mode != "sbx" || settings.Image != "example/mason:1" || settings.Dagger == nil {
				t.Fatalf("mason settings: %+v, %v", settings, err)
			}
			if settings.Dagger.Engine != tc.want || settings.Dagger.Version != "v0.20.5" {
				t.Fatalf("mason Dagger settings: %+v", settings.Dagger)
			}
			for _, role := range roleNames {
				if role == "mason" {
					continue
				}
				_, settings, err := cfg.Execution(role, "default")
				if err != nil || settings.Dagger != nil {
					t.Fatalf("%s received Dagger: %+v, %v", role, settings, err)
				}
			}
		})
	}
}
