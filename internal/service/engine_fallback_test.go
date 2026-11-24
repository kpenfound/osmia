package service

import (
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
)

func TestConfiguredCloudChecksBuildsTheFallbackRunnerOnlyWhenActive(t *testing.T) {
	const env, token = "OSMIA_FALLBACK_UNIT_TOKEN", "fallback-unit-secret"
	t.Setenv(env, token)
	cfg := func(engine, fallback string) *config.Config {
		return &config.Config{Dagger: config.DaggerConfig{Engine: engine, CloudComputeTokenEnv: env, CheckFallback: fallback}}
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

func TestConfiguredCloudRunnersUseTheirOwnTokens(t *testing.T) {
	const computeEnv, compute = "OSMIA_SPLIT_COMPUTE_TOKEN", "split-compute-secret"
	const apiEnv, apiToken = "OSMIA_SPLIT_API_TOKEN", "split-api-secret"
	t.Setenv(computeEnv, compute)
	t.Setenv(apiEnv, apiToken)
	cfg := &config.Config{Dagger: config.DaggerConfig{Engine: "managed", CloudComputeTokenEnv: computeEnv, CloudAPITokenEnv: apiEnv, CheckFallback: "cloud"}}
	s := &Service{}
	if got, want := s.configuredChecks(cfg), (checks.DaggerChecks{Engine: "docker-container://" + config.ManagedDaggerEngineName, CloudToken: compute}); got != want {
		t.Fatalf("check runner %#v, want %#v", got, want)
	}
	if got, want := s.configuredCloudChecks(cfg), (checks.DaggerChecks{Engine: "cloud", CloudToken: compute}); got != want {
		t.Fatalf("fallback runner %#v, want %#v", got, want)
	}
	if got, want := s.configuredRerun(cfg), (checks.DaggerChecks{CloudToken: apiToken}); got != want {
		t.Fatalf("rerun runner %#v, want %#v", got, want)
	}
	if got, want := s.configuredTrace(cfg), (checks.DaggerChecks{CloudToken: apiToken}); got != want {
		t.Fatalf("trace runner %#v, want %#v", got, want)
	}

	cfg.Dagger.CloudAPITokenEnv = ""
	if err := s.configuredRerun(cfg).CanRerun(); !errors.Is(err, checks.ErrCloudRerunUnavailable) {
		t.Fatalf("rerun without an API token: %v", err)
	}
	if got := s.configuredTrace(cfg); got != (checks.DaggerChecks{}) {
		t.Fatalf("trace runner without an API token %#v", got)
	}
}
