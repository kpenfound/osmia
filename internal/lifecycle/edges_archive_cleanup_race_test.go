package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

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
	cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(cleanup.Release)
	var armed, held atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var passes atomic.Int64
	host := &fixturekit.FakePulls{}
	f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{
		Wait: journey.LifecycleAwait,
		Options: func(opts *journey.Options) {
			opts.SchedulePassed = func() { passes.Add(1) }
		},
		Checkpoint: func(ctx context.Context, step string) error {
			if step == "edge-archive-retained-read" && armed.CompareAndSwap(true, false) {
				held.Store(true)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return cleanup.Checkpoint(ctx, step)
		},
	}, trace.EdgeHardBlock, host)
	defer f.Stop(t)
	defer resume()
	defer cleanup.Release()
	up, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	down, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: f.Repos[0].Upstream, Fork: f.S.Current().For(f.Project).Project.PushRepository(), Branch: intake.FeatureBranch(f.Stream1), Commit: "feature-head", PullRequest: 71}
	journey.RecordUpstreamOwnPullRequest(t, f.S, up, f.Stream1, p)
	journey.ResolutionDocument(t, up, f.Stream1, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: 70, Outcome: delivery.OwnPullRequestOutcomeMerged, MergeCommit: "stale-merge"})
	_, err = f.C.Archive(ctx, f.Stream1)
	fixturekit.Must(t, err)
	cleanup.Await(t)
	a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
	if !ok || !a.CleanupStartedAt.IsZero() || a.MergeCommit != "" || a.Publication.PullRequest != p.PullRequest {
		t.Fatalf("expected retained, unmerged current publication: %+v", a)
	}
	armed.Store(true)
	f.Clock.Advance(10 * time.Minute)
	journey.LifecycleAwait(t, f, "archived upstream retained read held", held.Load)
	before, beforeRevision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
	fixturekit.Must(t, err)
	for _, edge := range before.Edges {
		if edge.Merged {
			t.Fatal("stale earlier publication released the edge before cleanup")
		}
	}
	cleanup.Release()
	journey.LifecycleAwait(t, f, "cleanup closes and deletes upstream child", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("authorized cleanup did not delete upstream history")
	}
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: 70, Merged: true, MergeCommit: "stale-merge", HeadRepository: p.Fork, Head: p.Branch}, {Number: p.PullRequest, Merged: true, State: "closed", MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
	host.Mu.Unlock()
	resume()
	journey.LifecycleAwait(t, f, "edge recovers from concurrent cleanup", func() bool {
		obs, _, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
		fixturekit.Must(t, err)
		return len(obs.Edges) == 1 && obs.Edges[0].Merged
	})
	obs, revision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
	fixturekit.Must(t, err)
	if revision != beforeRevision+1 || obs.Edges[0].Commit != "current-merge" || obs.Edges[0].Reference != p.Upstream+"#71" || obs.Edges[0].BuildBranch != "" {
		t.Fatalf("edge lost exact archived publication identity: %+v", obs)
	}
	waiting, err := lifecycle.EdgeWaiting(down, f.Stream2)
	fixturekit.Must(t, err)
	if waiting {
		t.Fatal("verified current merge did not release hard block")
	}
	select {
	case <-f.S.Done:
		t.Fatalf("cleanup stopped service: %v", f.S.TestError())
	default:
	}
	f.Stop(t)
	f.Start(t)
	down, err = f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	pass := passes.Load()
	f.Clock.Advance(10 * time.Minute)
	journey.LifecycleAwait(t, f, "edge reconciliation after real restart", func() bool { return passes.Load() > pass+1 })
	after, afterRevision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
	fixturekit.Must(t, err)
	if revision != afterRevision || !slices.Equal(obs.Edges, after.Edges) {
		t.Fatalf("restart changed or duplicated release: %+v revision %d -> %d", after, revision, afterRevision)
	}
	gone, err = trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
	fixturekit.Must(t, err)
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
			cfg, repo, _, refresh := journey.OwnPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			fixturekit.Must(t, err)
			defer store.Close()
			s := refresh.S
			s.Store = store
			journey.DeliverWithPullRequest(t, repo, cfg, 42)
			if archived {
				if _, apiFault := s.Lifecycle().Archive(string(fixturekit.Stream)); apiFault != nil {
					t.Fatal(apiFault)
				}
			}
			fixturekit.TraceExec(t, fixturekit.WorkstreamTraceDirectory(t, cfg.Root, cfg.Project.ID, fixturekit.Stream), "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (1, ?, 'osmia.trace.document', 'corrupt', 1, 'corrupt history')", "workstreams/"+string(fixturekit.Stream)+"/documents.jsonl")
			status, err := s.Lifecycle().UpstreamMergeStatus(context.Background(), repo, trace.Edge{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeHardBlock})
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
			cfg, repo, host, refresh := journey.OwnPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			fixturekit.Must(t, err)
			defer store.Close()
			s := refresh.S
			s.Store = store
			p := journey.DeliverWithPullRequest(t, repo, cfg, 42)
			if _, apiFault := s.Lifecycle().Archive(string(fixturekit.Stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			if name == "no publication" {
				a, _ := s.Lifecycle().ArchivedWorkstream(string(fixturekit.Stream))
				a.Publication = runtime.ArchivePublication{}
				fixturekit.Must(t, store.ClearArchived(fixturekit.Stream))
				fixturekit.Must(t, store.SetArchived(a))
			}
			_, err = repo.MarkWorkstreamTraceForDeletion(fixturekit.Stream)
			fixturekit.Must(t, err)
			fixturekit.Must(t, trace.FinishWorkstreamTraceDeletion(cfg.Root, cfg.Project.ID, fixturekit.Stream))
			host.Prs = []pulls.PullRequest{{Number: 41, Merged: true, MergeCommit: "stale-merge", HeadRepository: p.Fork, Head: p.Branch}, {Number: 42, State: "closed", Merged: name == "merged", MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
			if name == "stale" {
				host.Prs = host.Prs[:1]
			}
			if name == "lookup failure" {
				host.FindErr = errors.New("provider unavailable")
			}
			status, err := s.Lifecycle().UpstreamMergeStatus(context.Background(), repo, trace.Edge{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeHardBlock})
			if name == "lookup failure" {
				if err == nil || status.Merged {
					t.Fatalf("failed lookup released dependency: %+v %v", status, err)
				}
			} else {
				fixturekit.Must(t, err)
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
			fixturekit.Must(t, err)
			if !gone {
				t.Fatal("lookup recreated deleted upstream history")
			}
		})
	}
}
