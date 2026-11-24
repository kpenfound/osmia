package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/jev"
)

// withTopLevel appends text to the root's top-level configuration.
func withTopLevel(t *testing.T, opts Options, text string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, text...), 0600))
}

// A service without Jev's API key starts and reports Jev as unconfigured;
// neither the status nor the configuration response carries an enabled switch.
func TestServiceStartsWithJevUnconfigured(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withTopLevel(t, opts, "\n[jev]\napi_key_env = \"OSMIA_TEST_UNSET_JEV_KEY\"\n")
	_, c := start(t, opts)
	ctx := context.Background()
	health, err := c.Health(ctx)
	must(t, err)
	if !health.Ready {
		t.Fatalf("service is not ready without a Jev key: %+v", health)
	}
	runtime, err := c.Runtime(ctx)
	must(t, err)
	if runtime.Jev.Mode != jev.ModeUnconfigured || runtime.Jev.Reason != jev.ReasonUnconfigured || !strings.Contains(runtime.Jev.Detail, "OSMIA_TEST_UNSET_JEV_KEY") {
		t.Fatalf("status = %+v", runtime.Jev)
	}
	config, err := c.Configuration(ctx)
	must(t, err)
	for name, v := range map[string]any{"status": runtime.Jev, "configuration": config.Effective.Jev} {
		raw, err := json.Marshal(v)
		must(t, err)
		var fields map[string]any
		must(t, json.Unmarshal(raw, &fields))
		if _, ok := fields["enabled"]; ok {
			t.Fatalf("%s response carries the removed switch: %s", name, raw)
		}
	}
}
