package fixturekit

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
)

// UnconfiguredJudge is Jev without its API key, as a fixture controller sees
// it by default: every judgment falls back, no request is sent and every check
// runs.
func UnconfiguredJudge(now func() time.Time) *jev.Judge {
	settings := config.Jev{URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "1s", CheckThreshold: config.DefaultJevCheckThreshold}
	return &jev.Judge{Config: func() config.Jev { return settings }, Getenv: func(string) string { return "" }, Now: now}
}
