package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
)

func (f *shedFixture) builtPaused(t *testing.T, target runtime.Target, keys ...string) []config.WorkstreamID {
	t.Helper()
	return intakefixture.BuiltPaused(t, target, keys, f.shedAs, func() { mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"}) }, f.ratifiedBuild)
}

func (f *shedFixture) shedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	return f.scenario().ShedAs(t, key)
}

func (f *shedFixture) ratifiedBuild(t *testing.T, stream config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	return f.scenario().RatifiedBuild(t, stream)
}
