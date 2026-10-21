package intakefixture

import (
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
)

// CheckJudge supplies deterministic smart-check selection for fixture controllers.
func CheckJudge(now func() time.Time, p *jevtest.Provider) *jev.Judge {
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "1s"}
	return &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: now}
}
