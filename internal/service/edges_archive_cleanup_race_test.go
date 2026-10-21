package service

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchivedUpstreamReadRecoversConcurrentCleanupAcrossRestart(t *testing.T) {
	ctx := context.Background()
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	var armed, held atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var passes atomic.Int64
	host := &fakePulls{}
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
	// Release checkpoints before stopping the service on a failed assertion.
	defer resume()
	defer cleanup.release()
	up, err := f.s.repository(f.project)
	must(t, err)
	down, err := f.s.repository(f.project2)
	must(t, err)
	p := DeliveryPublication{Status: publicationOpened, Upstream: f.repos[0].upstream, Fork: f.s.current().For(f.project).Project.PushRepository(), Branch: featureBranch(f.stream1), Commit: "feature-head", PullRequest: 71}
	recordUpstreamOwnPullRequest(t, f.s, up, f.stream1, p)
	// A stale earlier-PR merge must not be used by the interrupted read.
	resolutionDocument(t, up, f.stream1, ownPullRequestOutcomeDocument, ownPullRequestOutcomePath, ownPullRequestOutcome{PullRequest: 70, Outcome: ownPullRequestOutcomeMerged, MergeCommit: "stale-merge"})
	_, err = f.c.Archive(ctx, f.stream1)
	must(t, err)
	cleanup.await(t)
	a, ok := archiveRecord(t, f.s.store, f.stream1)
	if !ok || !a.CleanupStartedAt.IsZero() || a.MergeCommit != "" || a.Publication.PullRequest != p.PullRequest {
		t.Fatalf("expected retained, unmerged current publication: %+v", a)
	}
	// Workstream reads have cached the upstream child. Hold the dependent's
	// controller after archive lookup and enumeration, before retained reads.
	armed.Store(true)
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "archived upstream retained read held", held.Load)
	before, beforeRevision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	for _, edge := range before.Edges {
		if edge.Merged {
			t.Fatal("stale earlier publication released the edge before cleanup")
		}
	}
	cleanup.release()
	lifecycleAwait(t, f, "cleanup closes and deletes upstream child", func() bool {
		a, ok := archiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	must(t, err)
	if !gone {
		t.Fatal("authorized cleanup did not delete upstream history")
	}
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: 70, Merged: true, MergeCommit: "stale-merge", HeadRepository: p.Fork, Head: p.Branch}, {Number: p.PullRequest, Merged: true, State: "closed", MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
	host.Mu.Unlock()
	resume()
	lifecycleAwait(t, f, "edge recovers from concurrent cleanup", func() bool {
		obs, _, _, err := edgeObservationAt(down, f.stream2)
		must(t, err)
		return len(obs.Edges) == 1 && obs.Edges[0].Merged
	})
	obs, revision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if revision != beforeRevision+1 || obs.Edges[0].Commit != "current-merge" || obs.Edges[0].Reference != p.Upstream+"#71" || obs.Edges[0].BuildBranch != "" {
		t.Fatalf("edge lost exact archived publication identity: %+v", obs)
	}
	waiting, err := edgeWaiting(down, f.stream2)
	must(t, err)
	if waiting {
		t.Fatal("verified current merge did not release hard block")
	}
	select {
	case <-f.s.done:
		t.Fatalf("cleanup stopped service: %v", f.s.err)
	default:
	}
	f.stop(t)
	f.start(t)
	down, err = f.s.repository(f.project2)
	must(t, err)
	pass := passes.Load()
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "edge reconciliation after real restart", func() bool { return passes.Load() > pass+1 })
	after, afterRevision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if revision != afterRevision || !slices.Equal(obs.Edges, after.Edges) {
		t.Fatalf("restart changed or duplicated release: %+v revision %d -> %d", after, revision, afterRevision)
	}
	gone, err = trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	must(t, err)
	if !gone {
		t.Fatal("restart recreated upstream history")
	}
}

func TestUpstreamReadErrorsWithoutCleanupEvidence(t *testing.T) {
	for _, archived := range []bool{false, true} {
		name := "live"
		if archived {
			name = "archived-retained"
		}
		t.Run(name, func(t *testing.T) {
			cfg, repo, _, refresh := ownPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
			must(t, err)
			defer store.Close()
			s := refresh.s
			s.store = store
			deliverWithPullRequest(t, repo, cfg, 42)
			if archived {
				if _, api := s.archive(string(stream)); api != nil {
					t.Fatal(api)
				}
			}
			traceExec(t, workstreamTraceDirectory(t, cfg.Root, cfg.Project.ID, stream), "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (1, ?, 'osmia.trace.document', 'corrupt', 1, 'corrupt history')", "workstreams/"+string(stream)+"/documents.jsonl")
			status, err := s.upstreamMergeStatus(context.Background(), repo, trace.Edge{From: stream, To: quiet, Type: trace.EdgeHardBlock})
			if err == nil || status.Merged {
				t.Fatalf("unexplained corrupt upstream lost its error: %+v %v", status, err)
			}
		})
	}
}

func TestArchivedUpstreamGoneTraceUsesOnlyPinnedPublication(t *testing.T) {
	for _, name := range []string{"merged", "closed", "stale", "lookup failure", "no publication"} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, host, refresh := ownPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
			must(t, err)
			defer store.Close()
			s := refresh.s
			s.store = store
			p := deliverWithPullRequest(t, repo, cfg, 42)
			if _, api := s.archive(string(stream)); api != nil {
				t.Fatal(api)
			}
			if name == "no publication" {
				a, _ := s.archivedWorkstream(string(stream))
				a.Publication = runtime.ArchivePublication{}
				must(t, store.ClearArchived(stream))
				must(t, store.SetArchived(a))
			}
			// Retain the archive shape without cleanup timestamps, as a legacy
			// record may have, while removing only its authorized member trace.
			_, err = repo.MarkWorkstreamTraceForDeletion(stream)
			must(t, err)
			must(t, trace.FinishWorkstreamTraceDeletion(cfg.Root, cfg.Project.ID, stream))
			host.Prs = []pulls.PullRequest{{Number: 41, Merged: true, MergeCommit: "stale-merge", HeadRepository: p.Fork, Head: p.Branch}, {Number: 42, State: "closed", Merged: name == "merged", MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
			if name == "stale" {
				host.Prs = host.Prs[:1]
			}
			if name == "lookup failure" {
				host.FindErr = errors.New("provider unavailable")
			}
			status, err := s.upstreamMergeStatus(context.Background(), repo, trace.Edge{From: stream, To: quiet, Type: trace.EdgeHardBlock})
			if name == "lookup failure" {
				if err == nil || status.Merged {
					t.Fatalf("failed lookup released dependency: %+v %v", status, err)
				}
			} else {
				must(t, err)
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
			gone, err := trace.WorkstreamTraceGone(cfg.Root, cfg.Project.ID, stream)
			must(t, err)
			if !gone {
				t.Fatal("lookup recreated deleted upstream history")
			}
		})
	}
}
