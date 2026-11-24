package executionjourney

import (
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func builtPaused(f *journey.ShedFixture, t *testing.T, target runtime.Target, keys ...string) []config.WorkstreamID {
	t.Helper()
	return intakefixture.BuiltPaused(t, target, keys, f.ShedAs, func() {
		intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	}, f.RatifiedBuild)
}

func raisedContests(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID) []api.InboxEntry {
	t.Helper()
	var entries []api.InboxEntry
	intakefixture.Soon(t, "a contest to reach the inbox", func() bool {
		entries = f.InboxEntries(t, stream, api.InboxContested)
		return len(entries) != 0
	})
	return entries
}

func awaitDeferral(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit, reason string) {
	t.Helper()
	wait, close := journey.ServiceChanges(t, f.S)
	defer close()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	want := execution.DispatchDeferred + " " + reason
	for {
		decisions := dispatches(f, t, stream, unit)
		if len(decisions) > 0 && decisions[len(decisions)-1] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit %s dispatches %q, want %q", unit, decisions, want)
		}
		wait()
	}
}

func awaitCompleted(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, agent, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitCompleted(t, f.Repository(), stream, agent, turn)
}

func seedBuildingPaused(f *journey.ShedFixture, t *testing.T, target runtime.Target, graph string, keys ...string) []config.WorkstreamID {
	t.Helper()
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	var streams []config.WorkstreamID
	for _, key := range keys {
		streams = append(streams, seedBuilding(f, t, key, graph))
	}
	return streams
}
