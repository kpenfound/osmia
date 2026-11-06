package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
)

func TestConfiguredCloudChecksBuildsTheFallbackRunnerOnlyWhenActive(t *testing.T) {
	const env, token = "OSMIA_FALLBACK_UNIT_TOKEN", "fallback-unit-secret"
	t.Setenv(env, token)
	cfg := func(engine, fallback string) *config.Config {
		return &config.Config{Dagger: config.DaggerConfig{Engine: engine, CloudTokenEnv: env, CheckFallback: fallback}}
	}
	injected := checks.DaggerChecks{Engine: "injected"}
	for _, tc := range []struct {
		name         string
		cfg          *config.Config
		cloud        ReviewChecks
		primary      ReviewChecks
		wantFallback ReviewChecks
	}{
		{"off", cfg("auto", ""), nil, checks.DaggerChecks{Engine: "auto", CloudToken: token}, nil},
		{"off keeps an injected runner unused", cfg("auto", ""), injected, checks.DaggerChecks{Engine: "auto", CloudToken: token}, nil},
		{"on beside a managed engine", cfg("managed", "cloud"), nil, checks.DaggerChecks{Engine: "docker-container://" + config.ManagedDaggerEngineName, CloudToken: token}, checks.DaggerChecks{Engine: "cloud", CloudToken: token}},
		{"on beside the default engine", cfg("", "cloud"), nil, checks.DaggerChecks{Engine: "auto", CloudToken: token}, checks.DaggerChecks{Engine: "cloud", CloudToken: token}},
		{"on uses an injected runner", cfg("auto", "cloud"), injected, checks.DaggerChecks{Engine: "auto", CloudToken: token}, injected},
		{"cloud engine has no fallback runner", cfg("cloud", "cloud"), injected, checks.DaggerChecks{Engine: "cloud", CloudToken: token}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{options: Options{cloudChecks: tc.cloud}}
			if got := s.configuredCloudChecks(tc.cfg); got != tc.wantFallback {
				t.Fatalf("fallback runner %#v, want %#v", got, tc.wantFallback)
			}
			if got := s.configuredChecks(tc.cfg); got != tc.primary {
				t.Fatalf("primary runner %#v, want %#v", got, tc.primary)
			}
		})
	}
	if got := (&Service{}).configuredCloudChecks(nil); got != nil {
		t.Fatalf("a missing configuration built %#v", got)
	}
}
