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
		{"fallback on", "[dagger]\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = 'cloud'\n", ResolvedDagger{Mode: DaggerEngineAuto, CloudTokenEnv: "OSMIA_CHECK_TOKEN", CheckFallbackCloud: true}},
		{"fallback on managed", "[dagger]\nengine = 'managed'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = 'cloud'\n", ResolvedDagger{Mode: DaggerEngineManaged, Address: "docker-container://osmia-dagger-engine", CloudTokenEnv: "OSMIA_CHECK_TOKEN", CheckFallbackCloud: true}},
		{"fallback absent", "[dagger]\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\n", ResolvedDagger{Mode: DaggerEngineAuto, CloudTokenEnv: "OSMIA_CHECK_TOKEN"}},
		{"fallback empty", "[dagger]\ncheck_fallback = ''\n", ResolvedDagger{Mode: DaggerEngineAuto}},
		{"fallback empty with token", "[dagger]\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = ''\n", ResolvedDagger{Mode: DaggerEngineAuto, CloudTokenEnv: "OSMIA_CHECK_TOKEN"}},
		{"fallback with cloud engine", "[dagger]\nengine = 'cloud'\ncloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = 'cloud'\n", ResolvedDagger{Mode: DaggerEngineCloud, CloudTokenEnv: "OSMIA_CHECK_TOKEN"}},
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
		{"bad fallback", "check_fallback = 'private-secret'", "dagger.check_fallback"},
		{"bad fallback with token", "cloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = 'private-secret'", "dagger.check_fallback"},
		{"fallback case", "cloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = 'Cloud'", "dagger.check_fallback"},
		{"fallback local", "cloud_token_env = 'OSMIA_CHECK_TOKEN'\ncheck_fallback = 'local'", "dagger.check_fallback"},
		{"fallback without token", "check_fallback = 'cloud'", "dagger.cloud_token_env"},
		{"fallback empty token", "check_fallback = 'cloud'\ncloud_token_env = ''", "dagger.cloud_token_env"},
		{"wrong fallback type", "check_fallback = ['private-secret']", "dagger.check_fallback"},
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

func TestDaggerCheckFallbackInactiveForCloudEngine(t *testing.T) {
	d := DaggerConfig{Engine: "cloud", CloudTokenEnv: "OSMIA_CHECK_TOKEN", CheckFallback: "cloud"}
	if r := d.Resolve(); r.Mode != DaggerEngineCloud || r.CheckFallbackCloud {
		t.Fatalf("cloud engine with fallback: %+v", r)
	}
	d.Engine = "managed"
	if r := d.Resolve(); r.Mode != DaggerEngineManaged || !r.CheckFallbackCloud {
		t.Fatalf("managed engine with fallback: %+v", r)
	}
}
