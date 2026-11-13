package service

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// markAbandoned records the workstream as abandoned the way the owner's
// abandon does, without the rest of abandonment's effects.
func markAbandoned(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, s *Service) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: repository.Project(), Workstream: stream, At: s.now(), Actor: ownerActor, Cause: abandonTransition}
	_, err := repository.SetFeatureStateUnless(context.Background(), h, AbandonedState, "Superseded", AbandonedState, DeliveredState)
	must(t, err)
}

// A drift rebase held while its workstream waits for its base is released
// once the workstream is abandoned, while every other repository operation
// of the abandoned workstream stays held.
func TestAbandonedWorkstreamReleasesItsHeldDriftRebase(t *testing.T) {
	t.Parallel()
	cfg, repo, clock := hardPauseRepository(t, 0)
	s, _ := hardPauseService(t, cfg, repo, clock, nil)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: baseWaitSubject + "-waiting", Revision: 1, Project: repo.Project(), Workstream: stream, At: clock.Now(), Actor: foremanActor, Cause: baseWaitSubject}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: baseWaitSubject, To: "waiting", Reason: "the base workstream was abandoned"}})
	must(t, err)
	hold := s.holding(repo)
	drift := coreadapter.Operation{ID: "drift", Boundary: coreadapter.RepositoryBoundary, Action: DriftAction}
	land := coreadapter.Operation{ID: "land", Boundary: coreadapter.RepositoryBoundary, Action: LandAction}
	if !hold(stream, drift) || !hold(stream, land) {
		t.Fatal("a workstream waiting for its base is not held")
	}
	markAbandoned(t, repo, stream, s)
	if hold(stream, drift) {
		t.Fatal("the abandoned workstream's drift rebase is still held")
	}
	if !hold(stream, land) {
		t.Fatal("the abandoned workstream's landing is no longer held")
	}
}

// A drift rebase asked for while the workstream was building, and still
// pending when it is abandoned, settles as skipped without moving the
// feature branch, so it leaves no operation to defer archive cleanup.
func TestDriftRebaseOfAbandonedWorkstreamSettlesSkipped(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	stream, repository := seedBuild(t, f, "abandoned-drift", validPlan, config.WorkspacesGit, f.upstream(t))
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	op := requestDrift(t, d, stream)
	before := featureTip(t, f, stream)
	markAbandoned(t, repository, stream, f.s)
	if f.s.holding(repository)(stream, op) {
		t.Fatal("the abandoned workstream's drift rebase is held")
	}
	result := settleOperation(t, f.s, repository, stream, op, d)
	if result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "abandoned") {
		t.Fatalf("the abandoned workstream's drift rebase ended %+v", result)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the skipped drift rebase moved the feature branch from %s to %s", before, tip)
	}
	pending, err := (&workspaceCleanup{Cfg: f.s.cfg, Repository: repository}).Pending(stream)
	must(t, err)
	if pending {
		t.Fatal("the abandoned workstream still has an operation without a result")
	}
}
