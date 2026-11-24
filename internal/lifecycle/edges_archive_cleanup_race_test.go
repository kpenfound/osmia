package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchivedUpstreamReadRecoversConcurrentCleanupAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	var armed, held atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var passes atomic.Int64
	host := &fixturekit.FakePulls{}
	f := resolutionApprovedFixture(t, productionDesignConfiguration{
		wait: lifecycleAwait,
		options: func(opts *Options) {
			opts.schedulePassed = func() { passes.Add(1) }
		},
		checkpoint: func(ctx context.Context, step string) error {
			if step == "edge-archive-retained-read" && armed.CompareAndSwap(true, false) {
				held.Store(true)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return cleanup.checkpoint(ctx, step)
		},
	}, trace.EdgeHardBlock, host)
	defer f.stop(t)
	defer resume()
	defer cleanup.release()
	up, err := f.s.repository(f.project)
	intakefixture.Must(t, err)
	down, err := f.s.repository(f.project2)
	intakefixture.Must(t, err)
	p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: f.repos[0].Upstream, Fork: f.s.current().For(f.project).Project.PushRepository(), Branch: intake.FeatureBranch(f.stream1), Commit: "feature-head", PullRequest: 71}
	recordUpstreamOwnPullRequest(t, f.s, up, f.stream1, p)
	resolutionDocument(t, up, f.stream1, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: 70, Outcome: delivery.OwnPullRequestOutcomeMerged, MergeCommit: "stale-merge"})
	_, err = f.c.Archive(ctx, f.stream1)
	intakefixture.Must(t, err)
	cleanup.await(t)
	a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
	if !ok || !a.CleanupStartedAt.IsZero() || a.MergeCommit != "" || a.Publication.PullRequest != p.PullRequest {
		t.Fatalf("expected retained, unmerged current publication: %+v", a)
	}
	armed.Store(true)
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "archived upstream retained read held", held.Load)
	before, beforeRevision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	intakefixture.Must(t, err)
	for _, edge := range before.Edges {
		if edge.Merged {
			t.Fatal("stale earlier publication released the edge before cleanup")
		}
	}
	cleanup.release()
	lifecycleAwait(t, f, "cleanup closes and deletes upstream child", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	intakefixture.Must(t, err)
	if !gone {
		t.Fatal("authorized cleanup did not delete upstream history")
	}
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: 70, Merged: true, MergeCommit: "stale-merge", HeadRepository: p.Fork, Head: p.Branch}, {Number: p.PullRequest, Merged: true, State: "closed", MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
	host.Mu.Unlock()
	resume()
	lifecycleAwait(t, f, "edge recovers from concurrent cleanup", func() bool {
		obs, _, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
		intakefixture.Must(t, err)
		return len(obs.Edges) == 1 && obs.Edges[0].Merged
	})
	obs, revision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	intakefixture.Must(t, err)
	if revision != beforeRevision+1 || obs.Edges[0].Commit != "current-merge" || obs.Edges[0].Reference != p.Upstream+"#71" || obs.Edges[0].BuildBranch != "" {
		t.Fatalf("edge lost exact archived publication identity: %+v", obs)
	}
	waiting, err := lifecycle.EdgeWaiting(down, f.stream2)
	intakefixture.Must(t, err)
	if waiting {
		t.Fatal("verified current merge did not release hard block")
	}
	select {
	case <-f.s.done:
		t.Fatalf("cleanup stopped service: %v", f.s.testError())
	default:
	}
	f.stop(t)
	f.start(t)
	down, err = f.s.repository(f.project2)
	intakefixture.Must(t, err)
	pass := passes.Load()
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "edge reconciliation after real restart", func() bool { return passes.Load() > pass+1 })
	after, afterRevision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	intakefixture.Must(t, err)
	if revision != afterRevision || !slices.Equal(obs.Edges, after.Edges) {
		t.Fatalf("restart changed or duplicated release: %+v revision %d -> %d", after, revision, afterRevision)
	}
	gone, err = trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	intakefixture.Must(t, err)
	if !gone {
		t.Fatal("restart recreated upstream history")
	}
}
func TestUpstreamReadErrorsWithoutCleanupEvidence(t *testing.T) {
	t.Parallel()
	for _, archived := range []bool{false, true} {
		name := "live"
		if archived {
			name = "archived-retained"
		}
		t.Run(name, func(t *testing.T) {
			cfg, repo, _, refresh := ownPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			intakefixture.Must(t, err)
			defer store.Close()
			s := refresh.s
			s.store = store
			deliverWithPullRequest(t, repo, cfg, 42)
			if archived {
				if _, apiFault := s.archive(string(fixturekit.Stream)); apiFault != nil {
					t.Fatal(apiFault)
				}
			}
			fixturekit.TraceExec(t, intakefixture.WorkstreamTraceDirectory(t, cfg.Root, cfg.Project.ID, fixturekit.Stream), "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (1, ?, 'osmia.trace.document', 'corrupt', 1, 'corrupt history')", "workstreams/"+string(fixturekit.Stream)+"/documents.jsonl")
			status, err := s.upstreamMergeStatus(context.Background(), repo, trace.Edge{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeHardBlock})
			if err == nil || status.Merged {
				t.Fatalf("unexplained corrupt upstream lost its error: %+v %v", status, err)
			}
		})
	}
}
func TestArchivedUpstreamGoneTraceUsesOnlyPinnedPublication(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"merged", "closed", "stale", "lookup failure", "no publication"} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, host, refresh := ownPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			intakefixture.Must(t, err)
			defer store.Close()
			s := refresh.s
			s.store = store
			p := deliverWithPullRequest(t, repo, cfg, 42)
			if _, apiFault := s.archive(string(fixturekit.Stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			if name == "no publication" {
				a, _ := s.archivedWorkstream(string(fixturekit.Stream))
				a.Publication = runtime.ArchivePublication{}
				intakefixture.Must(t, store.ClearArchived(fixturekit.Stream))
				intakefixture.Must(t, store.SetArchived(a))
			}
			_, err = repo.MarkWorkstreamTraceForDeletion(fixturekit.Stream)
			intakefixture.Must(t, err)
			intakefixture.Must(t, trace.FinishWorkstreamTraceDeletion(cfg.Root, cfg.Project.ID, fixturekit.Stream))
			host.Prs = []pulls.PullRequest{{Number: 41, Merged: true, MergeCommit: "stale-merge", HeadRepository: p.Fork, Head: p.Branch}, {Number: 42, State: "closed", Merged: name == "merged", MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
			if name == "stale" {
				host.Prs = host.Prs[:1]
			}
			if name == "lookup failure" {
				host.FindErr = errors.New("provider unavailable")
			}
			status, err := s.upstreamMergeStatus(context.Background(), repo, trace.Edge{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeHardBlock})
			if name == "lookup failure" {
				if err == nil || status.Merged {
					t.Fatalf("failed lookup released dependency: %+v %v", status, err)
				}
			} else {
				intakefixture.Must(t, err)
				if status.Merged != (name == "merged") {
					t.Fatalf("absence or stale publication released dependency: %+v", status)
				}
				if name == "merged" && (status.Commit != "current-merge" || status.Reference != p.Upstream+"#42") {
					t.Fatalf("wrong merge identity: %+v", status)
				}
				if name == "no publication" && host.Finds != 0 {
					t.Fatal("missing publication caused an unpinned lookup")
				}
			}
			gone, err := trace.WorkstreamTraceGone(cfg.Root, cfg.Project.ID, fixturekit.Stream)
			intakefixture.Must(t, err)
			if !gone {
				t.Fatal("lookup recreated deleted upstream history")
			}
		})
	}
}
