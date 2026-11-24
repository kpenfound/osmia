package lifecycle_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchivedUpstreamMergeReleasesEdgeAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, edge        string
		detected, cleaned bool
	}{
		{name: "owner-retained", edge: trace.EdgeHardBlock},
		{name: "owner-cleaned", edge: trace.EdgeHardBlock, cleaned: true},
		{name: "detected-retained", edge: trace.EdgeMergeOrder, detected: true},
		{name: "detected-cleaned", edge: trace.EdgeMergeOrder, detected: true, cleaned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
			t.Cleanup(cleanup.Release)
			var interrupt, observed atomic.Bool
			interrupt.Store(true)
			host := &fixturekit.FakePulls{}
			f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
				if step == "edge-merge-observed" && interrupt.Load() {
					observed.Store(true)
					<-ctx.Done()
					return ctx.Err()
				}
				return cleanup.Checkpoint(ctx, step)
			}}, tc.edge, host)
			defer f.Stop(t)
			up, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			down, err := f.S.Repository(f.Project2)
			fixturekit.Must(t, err)
			publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: f.Repos[0].Upstream, Fork: f.S.Current().For(f.Project).Project.PushRepository(), Branch: intake.FeatureBranch(f.Stream1), Commit: "feature-head", PullRequest: 71, URL: "https://example.invalid/pull/71"}
			journey.RecordUpstreamOwnPullRequest(t, f.S, up, f.Stream1, publication)
			if tc.edge == trace.EdgeHardBlock {
				held, err := f.S.Holds(f.Project2, down, scheduler.Candidate{Workstream: f.Stream2, Thread: trace.Thread{Identity: trace.Agent{Role: intake.MasonRole}}})
				fixturekit.Must(t, err)
				if !held {
					t.Fatal("hard block admitted a mason before merge")
				}
				threads, err := down.Threads(f.Stream2)
				fixturekit.Must(t, err)
				for _, thread := range threads {
					if thread.Identity.Role == intake.MasonRole {
						t.Fatal("hard block started a unit before merge")
					}
				}
			}
			if tc.detected {
				host.Mu.Lock()
				host.Prs = []pulls.PullRequest{{Number: 71, State: "closed", Merged: true, MergeCommit: "merged-upstream", HeadRepository: publication.Fork, Head: publication.Branch}}
				host.Mu.Unlock()
				journey.LifecycleAwait(t, f, "own merge detection", func() bool {
					o, rev, err := delivery.OwnPullRequestOutcomeAt(up, f.Stream1)
					fixturekit.Must(t, err)
					return rev > 0 && o.Outcome == delivery.OwnPullRequestOutcomeMerged
				})
			} else {
				journey.RecordResolutionMerge(t, up, f.Stream1)
			}
			journey.LifecycleAwait(t, f, "edge merge read before persistence", observed.Load)
			_, err = f.C.Archive(ctx, f.Stream1)
			fixturekit.Must(t, err)
			cleanup.Await(t)
			a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
			if !ok || a.MergeCommit != "merged-upstream" || a.MergeReference == "" {
				t.Fatalf("archive lost merge identity: %+v", a)
			}
			before, _, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
			fixturekit.Must(t, err)
			for _, edge := range before.Edges {
				if edge.Merged {
					t.Fatal("dependent observed merge before archive/restart")
				}
			}
			if tc.cleaned {
				cleanup.Release()
				journey.LifecycleAwait(t, f, "upstream cleanup", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
					return ok && !a.CleanedAt.IsZero()
				})
			}
			f.Stop(t)
			interrupt.Store(false)
			f.Start(t)
			down, err = f.S.Repository(f.Project2)
			fixturekit.Must(t, err)
			journey.LifecycleAwait(t, f, "archived upstream edge resolution", func() bool {
				obs, _, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
				fixturekit.Must(t, err)
				return len(obs.Edges) == 1 && obs.Edges[0].Merged
			})
			obs, revision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
			fixturekit.Must(t, err)
			if obs.Edges[0].Commit != "merged-upstream" || obs.Edges[0].BuildBranch != "" || obs.Edges[0].Reference == "" {
				t.Fatalf("wrong dependency identity: %+v", obs)
			}
			waiting, err := lifecycle.EdgeWaiting(down, f.Stream2)
			fixturekit.Must(t, err)
			if waiting {
				t.Fatal("hard block remained stuck after archived merge")
			}
			f.Stop(t)
			f.Start(t)
			sent, err := f.C.Send(ctx, f.Stream2, "Report the dependency status after restart.")
			fixturekit.Must(t, err)
			journey.LifecycleAwait(t, f, "survivor chief dispatch after restart", func() bool {
				conversation, err := f.C.Conversation(ctx, f.Stream2)
				fixturekit.Must(t, err)
				for _, entry := range conversation.Entries {
					if entry.Turn == sent.Turn && entry.Kind == "response" && entry.State == api.TurnDone {
						return true
					}
				}
				return false
			})
			down, err = f.S.Repository(f.Project2)
			fixturekit.Must(t, err)
			after, afterRevision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
			fixturekit.Must(t, err)
			if afterRevision != revision || len(after.Edges) != 1 || after.Edges[0] != obs.Edges[0] {
				t.Fatalf("restart duplicated or changed merge receipt: %+v revision %d -> %d", after, revision, afterRevision)
			}

			if tc.cleaned {
				gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
				fixturekit.Must(t, err)
				if !gone {
					t.Fatal("edge lookup recreated archived history")
				}
			}
		})
	}
}
func TestArchivedUpstreamRequiresCurrentPullRequestMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                              string
		cleaned, merged, closed, failed, stale, cancelled bool
	}{
		{name: "retained open"},
		{name: "retained closed", closed: true},
		{name: "retained stale outcome", stale: true},
		{name: "cleaned open", cleaned: true},
		{name: "cleaned cancelled", cleaned: true, cancelled: true},
		{name: "cleaned provider failure", cleaned: true, failed: true},
		{name: "cleaned current merge", cleaned: true, merged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, host, refresh := journey.OwnPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			fixturekit.Must(t, err)
			defer store.Close()
			s := refresh.S
			s.Store = store
			p := journey.DeliverWithPullRequest(t, repo, cfg, 42)
			if tc.stale {
				journey.ResolutionDocument(t, repo, fixturekit.Stream, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: 41, Outcome: delivery.OwnPullRequestOutcomeMerged, MergeCommit: "old-merge"})
			}
			if _, apiFault := s.Lifecycle().Archive(string(fixturekit.Stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			a, ok := s.Lifecycle().ArchivedWorkstream(string(fixturekit.Stream))
			if !ok {
				t.Fatal("archive missing")
			}
			if tc.cleaned {
				a.CleanupStartedAt = s.Now()
				a.CleanedAt = s.Now()
				if tc.cancelled {
					a.Publication = runtime.ArchivePublication{}
					a.State = intake.AbandonedState
					a.Resolved = true
					a.ResolutionEvidence = "cancelled"
				}
				if tc.cancelled {
					fixturekit.Must(t, store.ClearArchived(fixturekit.Stream))
					fixturekit.Must(t, store.SetArchived(a))
				} else {
					fixturekit.Must(t, store.UpdateArchive(a))
				}
			}
			host.Prs = []pulls.PullRequest{{Number: 41, Merged: true, MergeCommit: "wrong-pr", HeadRepository: p.Fork, Head: p.Branch}, {Number: 42, State: "open", Merged: tc.merged, MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
			if tc.closed {
				host.Prs[1].State = "closed"
			}
			if tc.failed {
				host.FindErr = errors.New("lookup unavailable")
			}
			status, err := s.Lifecycle().UpstreamMergeStatus(ctx, repo, trace.Edge{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeHardBlock})
			if tc.failed {
				if err == nil || status.Merged {
					t.Fatalf("failed lookup released dependency: %+v %v", status, err)
				}
				return
			}
			fixturekit.Must(t, err)
			if status.Merged != tc.merged {
				t.Fatalf("archive released without current merge: %+v", status)
			}
			if tc.merged && (status.Commit != "current-merge" || status.Reference != p.Upstream+"#42" || status.BuildBranch != "") {
				t.Fatalf("wrong archived dependency identity: %+v", status)
			}
		})
	}
}
