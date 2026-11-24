package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/jev"
)

func withTopLevel(t *testing.T, opts Options, text string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, append(top, text...), 0600))
}

func startWithoutJevKey(t *testing.T) *Client {
	t.Helper()
	opts := fixture(t)
	withTopLevel(t, opts, "\n[jev]\napi_key_env = \"OSMIA_TEST_UNSET_JEV_KEY\"\n")
	_, c := start(t, opts)
	return c
}

func TestServiceIsReadyWithoutJevsAPIKey(t *testing.T) {
	t.Parallel()
	health, err := startWithoutJevKey(t).Health(context.Background())
	fixturekit.Must(t, err)
	if !health.Ready {
		t.Fatalf("service is not ready without a Jev key: %+v", health)
	}
}

func TestRuntimeReportsJevUnconfiguredAndNamesTheMissingVariable(t *testing.T) {
	t.Parallel()
	runtime, err := startWithoutJevKey(t).Runtime(context.Background())
	fixturekit.Must(t, err)
	if runtime.Jev.Mode != jev.ModeUnconfigured || runtime.Jev.Reason != jev.ReasonUnconfigured || !strings.Contains(runtime.Jev.Detail, "OSMIA_TEST_UNSET_JEV_KEY") {
		t.Fatalf("status = %+v", runtime.Jev)
	}
}
