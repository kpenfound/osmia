package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *shedFixture) builtPaused(t *testing.T, target runtime.Target, keys ...string) []config.WorkstreamID {
	t.Helper()
	return intakefixture.BuiltPaused(t, target, keys, f.shedAs, func() {
		intakefixture.Mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	}, f.ratifiedBuild)
}

func (f *shedFixture) shedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	return f.scenario().ShedAs(t, key)
}

func (f *shedFixture) ratifiedBuild(t *testing.T, stream config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	return f.scenario().RatifiedBuild(t, stream)
}

func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.scenario().BuiltAs(t, key)
}
