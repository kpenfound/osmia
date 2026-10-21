package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// edgeGraphFixture is a stopped, single-project service with two
// workstreams connected by a single workgroup graph edge, ready for the
// edge-resolution tests below. The service is stopped, as the same-repo
// base-dependency tests it mirrors are (base_upstream_test.go), so that
// nothing but the test itself drives edgeResolverPass and edgeWaitPass: a
// live reconcile loop would otherwise run them concurrently on its own
// schedule and race the test's own calls. It is the "test fixture...
// records the project spec, graph and slices" workgroup-spec's own guidance
// sanctions (plan#workgroup-spec), reused here since edge resolution only
// reads what CreateWorkgroup and RecordProjectSpec already record.
type edgeGraphFixture struct {
	s          *Service
	clock      *demoClock
	project    config.ProjectID
	repository *trace.Repository
	upstream   config.WorkstreamID
	downstream config.WorkstreamID
}

func newEdgeGraphFixture(t *testing.T, edgeType string) *edgeGraphFixture {
	t.Helper()
	f := newDebateFixture(t, 1, 1)
	f.stop(t)
	repository, upstream, downstream := fixturekit.EdgeGraph(t, f.s.cfg.Root, f.s.cfg.Project, edgeType, f.s.now(), registrationActor, ownerActor)
	return &edgeGraphFixture{s: f.s, clock: f.clock, project: f.project, repository: repository, upstream: upstream, downstream: downstream}
}

// resolvePasses runs both edge passes once, the way the reconcile controller
// would on its own schedule: edgeResolverPass refreshes the observation of
// every incoming edge, and edgeWaitPass keeps edgeWaitSubject current from
// it. It advances the fixture's clock well past edgeResolverPass's configured polling
// interval first, so each call sees the latest recorded state
// rather than skipping it as a too-recent repeat.
func (f *edgeGraphFixture) resolvePasses(t *testing.T) {
	t.Helper()
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(11 * time.Minute)
	f.clock.Mu.Unlock()
	must(t, f.s.edgeResolverPass(context.Background(), f.repository))
	must(t, f.s.edgeWaitPass(context.Background(), f.repository))
}

// inboxLen is the number of entries ever escalated to the owner.
func (f *edgeGraphFixture) inboxLen(t *testing.T) int {
	t.Helper()
	entries, err := f.repository.Inbox()
	must(t, err)
	return len(entries)
}
