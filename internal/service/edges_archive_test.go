package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchivedUpstreamMergeReleasesEdgeAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name, edge                string
		detected, cleaned, legacy bool
	}{
		{name: "owner-retained", edge: trace.EdgeHardBlock},
		{name: "owner-cleaned", edge: trace.EdgeHardBlock, cleaned: true},
		{name: "detected-retained", edge: trace.EdgeMergeOrder, detected: true},
		{name: "detected-cleaned", edge: trace.EdgeMergeOrder, detected: true, cleaned: true},
		{name: "legacy-retained", edge: trace.EdgeHardBlock, detected: true, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
			t.Cleanup(cleanup.release)
			var interrupt, observed atomic.Bool
			interrupt.Store(true)
			host := &fakePulls{}
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
				if step == "edge-merge-observed" && interrupt.Load() {
					observed.Store(true)
					<-ctx.Done()
					return ctx.Err()
				}
				return cleanup.checkpoint(ctx, step)
			}}, tc.edge, host)
			defer f.stop(t)
			up, err := f.s.repository(f.project)
			must(t, err)
			down, err := f.s.repository(f.project2)
			must(t, err)
			publication := DeliveryPublication{Status: publicationOpened, Upstream: f.repos[0].Upstream, Fork: f.s.current().For(f.project).Project.PushRepository(), Branch: featureBranch(f.stream1), Commit: "feature-head", PullRequest: 71, URL: "https://example.invalid/pull/71"}
			recordUpstreamOwnPullRequest(t, f.s, up, f.stream1, publication)
			if tc.edge == trace.EdgeHardBlock {
				held, err := f.s.holds(f.project2, librarianWorkstream(f.project2), down, scheduler.Candidate{Workstream: f.stream2, Thread: trace.Thread{Identity: trace.Agent{Role: masonRole}}})
				must(t, err)
				if !held {
					t.Fatal("hard block admitted a mason before merge")
				}
				threads, err := down.Threads(f.stream2)
				must(t, err)
				for _, thread := range threads {
					if thread.Identity.Role == masonRole {
						t.Fatal("hard block started a unit before merge")
					}
				}
			}
			if tc.detected {
				host.Mu.Lock()
				host.Prs = []pulls.PullRequest{{Number: 71, State: "closed", Merged: true, MergeCommit: "merged-upstream", HeadRepository: publication.Fork, Head: publication.Branch}}
				host.Mu.Unlock()
				lifecycleAwait(t, f, "own merge detection", func() bool {
					o, rev, err := ownPullRequestOutcomeAt(up, f.stream1)
					must(t, err)
					return rev > 0 && o.Outcome == ownPullRequestOutcomeMerged
				})
			} else {
				recordResolutionMerge(t, up, f.stream1)
			}
			lifecycleAwait(t, f, "edge merge read before persistence", observed.Load)
			// The controller has read the merge, but interruption prevents the
			// dependent from recording it. Archive must not lose this release event.
			_, err = f.c.Archive(ctx, f.stream1)
			must(t, err)
			cleanup.await(t)
			a, ok := archiveRecord(t, f.s.store, f.stream1)
			if !ok || a.MergeCommit != "merged-upstream" || a.MergeReference == "" {
				t.Fatalf("archive lost merge identity: %+v", a)
			}
			before, _, _, err := edgeObservationAt(down, f.stream2)
			must(t, err)
			for _, edge := range before.Edges {
				if edge.Merged {
					t.Fatal("dependent observed merge before archive/restart")
				}
			}
			if tc.cleaned {
				cleanup.release()
				lifecycleAwait(t, f, "upstream cleanup", func() bool { a, ok := archiveRecord(t, f.s.store, f.stream1); return ok && !a.CleanedAt.IsZero() })
			}
			f.stop(t)
			if tc.legacy {
				path := filepath.Join(f.opts.Config.Root, "runtime.json")
				data, err := os.ReadFile(path)
				must(t, err)
				var state map[string]any
				must(t, json.Unmarshal(data, &state))
				for _, raw := range state["archived"].([]any) {
					a := raw.(map[string]any)
					if a["workstream"] == string(f.stream1) {
						delete(a, "merge_commit")
						delete(a, "merge_reference")
						delete(a, "publication")
					}
				}
				data, err = json.Marshal(state)
				must(t, err)
				must(t, os.WriteFile(path, data, 0600))
			}
			interrupt.Store(false)
			f.start(t)
			down, err = f.s.repository(f.project2)
			must(t, err)
			lifecycleAwait(t, f, "archived upstream edge resolution", func() bool {
				obs, _, _, err := edgeObservationAt(down, f.stream2)
				must(t, err)
				return len(obs.Edges) == 1 && obs.Edges[0].Merged
			})
			obs, revision, _, err := edgeObservationAt(down, f.stream2)
			must(t, err)
			if obs.Edges[0].Commit != "merged-upstream" || obs.Edges[0].BuildBranch != "" || obs.Edges[0].Reference == "" {
				t.Fatalf("wrong dependency identity: %+v", obs)
			}
			waiting, err := edgeWaiting(down, f.stream2)
			must(t, err)
			if waiting {
				t.Fatal("hard block remained stuck after archived merge")
			}
			f.stop(t)
			f.start(t)
			sent, err := f.c.Send(ctx, f.stream2, "Report the dependency status after restart.")
			must(t, err)
			lifecycleAwait(t, f, "survivor chief dispatch after restart", func() bool {
				conversation, err := f.c.Conversation(ctx, f.stream2)
				must(t, err)
				for _, entry := range conversation.Entries {
					if entry.Turn == sent.Turn && entry.Kind == "response" && entry.State == TurnDone {
						return true
					}
				}
				return false
			})
			down, err = f.s.repository(f.project2)
			must(t, err)
			after, afterRevision, _, err := edgeObservationAt(down, f.stream2)
			must(t, err)
			if afterRevision != revision || len(after.Edges) != 1 || after.Edges[0] != obs.Edges[0] {
				t.Fatalf("restart duplicated or changed merge receipt: %+v revision %d -> %d", after, revision, afterRevision)
			}

			if tc.cleaned {
				gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
				must(t, err)
				if !gone {
					t.Fatal("edge lookup recreated archived history")
				}
			}
		})
	}
}

func TestArchivedUpstreamRequiresCurrentPullRequestMerge(t *testing.T) {
	for _, tc := range []struct {
		name                                                      string
		cleaned, merged, closed, failed, stale, legacy, cancelled bool
	}{
		{name: "retained open"},
		{name: "retained closed", closed: true},
		{name: "retained stale outcome", stale: true},
		{name: "cleaned open", cleaned: true},
		{name: "cleaned cancelled", cleaned: true, cancelled: true},
		{name: "cleaned legacy resolved flag", cleaned: true, legacy: true},
		{name: "cleaned provider failure", cleaned: true, failed: true},
		{name: "cleaned current merge", cleaned: true, merged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, host, refresh := ownPRFixture(t)
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
			must(t, err)
			defer store.Close()
			s := refresh.s
			s.store = store
			p := deliverWithPullRequest(t, repo, cfg, 42)
			if tc.stale {
				resolutionDocument(t, repo, stream, ownPullRequestOutcomeDocument, ownPullRequestOutcomePath, ownPullRequestOutcome{PullRequest: 41, Outcome: ownPullRequestOutcomeMerged, MergeCommit: "old-merge"})
			}
			if _, api := s.archive(string(stream)); api != nil {
				t.Fatal(api)
			}
			a, ok := s.archivedWorkstream(string(stream))
			if !ok {
				t.Fatal("archive missing")
			}
			if tc.cleaned {
				a.CleanupStartedAt = s.now()
				a.CleanedAt = s.now()
				if tc.legacy {
					a.Publication = runtime.ArchivePublication{}
					a.Resolved = true
					a.ResolutionEvidence = ""
				}
				if tc.cancelled {
					a.Publication = runtime.ArchivePublication{}
					a.State = AbandonedState
					a.Resolved = true
					a.ResolutionEvidence = "cancelled"
				}
				// Legacy records predate the publication pin; create a fresh store view
				// for those cases rather than mutating immutable production evidence.
				if tc.legacy || tc.cancelled {
					must(t, store.ClearArchived(stream))
					must(t, store.SetArchived(a))
				} else {
					must(t, store.UpdateArchive(a))
				}
			}
			host.Prs = []pulls.PullRequest{{Number: 41, Merged: true, MergeCommit: "wrong-pr", HeadRepository: p.Fork, Head: p.Branch}, {Number: 42, State: "open", Merged: tc.merged, MergeCommit: "current-merge", HeadRepository: p.Fork, Head: p.Branch}}
			if tc.closed {
				host.Prs[1].State = "closed"
			}
			if tc.failed {
				host.FindErr = errors.New("lookup unavailable")
			}
			status, err := s.upstreamMergeStatus(ctx, repo, trace.Edge{From: stream, To: quiet, Type: trace.EdgeHardBlock})
			if tc.failed {
				if err == nil || status.Merged {
					t.Fatalf("failed lookup released dependency: %+v %v", status, err)
				}
				return
			}
			must(t, err)
			if status.Merged != tc.merged {
				t.Fatalf("archive released without current merge: %+v", status)
			}
			if tc.merged && (status.Commit != "current-merge" || status.Reference != p.Upstream+"#42" || status.BuildBranch != "") {
				t.Fatalf("wrong archived dependency identity: %+v", status)
			}
		})
	}
}
