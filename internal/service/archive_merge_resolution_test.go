package service

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDesignProductionPostCleanupMergeRetiresProjectSpecAcrossRestart(t *testing.T) {
	ctx := context.Background()
	host := &fakePulls{}
	var interrupt, reached atomic.Bool
	interrupt.Store(true)
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
		if (step == "archive-edge-merge-resolution" || step == "archive-own-pr-merge-observed") && interrupt.Load() {
			if step == "archive-edge-merge-resolution" {
				reached.Store(true)
			}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}, trace.EdgeHardBlock, host)
	defer f.stop(t)
	publications := deliverResolutionMembers(t, f)
	up, err := f.s.repository(f.project)
	must(t, err)
	group, err := up.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	check := func(want bool) {
		t.Helper()
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
		must(t, err)
		defer authority.Close()
		ids, rev, err := trace.ResolvedWorkgroups(authority)
		must(t, err)
		active, err := trace.ProjectSpecActive(authority, group)
		must(t, err)
		if want {
			if !slices.Equal(ids, []config.WorkgroupID{group}) || rev != 1 || active {
				t.Fatalf("group=%v rev=%d active=%v", ids, rev, active)
			}
		} else if len(ids) != 0 || rev != 0 || !active {
			t.Fatal("project spec retired before every member merged")
		}
		for _, role := range []string{masonRole, trace.ChiefOfStaff} {
			b, err := f.s.Context().Assemble(ctx, f.project2, bundle.Scope{Role: role, Workstream: f.stream2})
			must(t, err)
			if (b.ProjectSpec != nil) == want {
				t.Fatalf("role %s spec presence=%v resolved=%v", role, b.ProjectSpec != nil, want)
			}
		}
		spec, found, err := trace.ProjectSpecFor(authority, group)
		must(t, err)
		if !found || spec.Behavior == "" {
			t.Fatal("shared spec history lost")
		}
	}
	_, err = f.c.Archive(ctx, f.stream1)
	must(t, err)
	lifecycleAwait(t, f, "unmerged upstream cleanup", func() bool { a, ok := archiveRecord(t, f.s.store, f.stream1); return ok && !a.CleanedAt.IsZero() })
	before, ok := archiveRecord(t, f.s.store, f.stream1)
	if !ok || before.Resolved || before.ResolutionEvidence != "unresolved" || before.Publication.PullRequest != publications[0].PullRequest {
		t.Fatalf("archive before merge=%+v", before)
	}
	check(false)
	// Only the exact archived current PR may supply the post-cleanup merge.
	p := publications[0]
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: "upstream-merge", Head: p.Branch, HeadRepository: p.Fork}}
	host.Mu.Unlock()
	lifecycleAwait(t, f, "durable merge receipt before archive reconciliation", reached.Load)
	down, err := f.s.repository(f.project2)
	must(t, err)
	obs, revision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if revision == 0 || len(obs.Edges) != 1 || !obs.Edges[0].Merged || obs.Edges[0].Commit != "upstream-merge" {
		t.Fatalf("receipt=%+v rev=%d", obs, revision)
	}
	a, _ := archiveRecord(t, f.s.store, f.stream1)
	if a.Resolved {
		t.Fatal("interruption did not precede archive reconciliation")
	}
	check(false)
	f.stop(t)
	interrupt.Store(false)
	// Recovery must replay the durable receipt even when polling can no longer
	// supply that upstream merge.
	host.Mu.Lock()
	host.Prs = nil
	host.Mu.Unlock()
	f.start(t)
	lifecycleAwait(t, f, "recovered archived resolution", func() bool { a, ok := archiveRecord(t, f.s.store, f.stream1); return ok && archiveResolved(a) })
	a, _ = archiveRecord(t, f.s.store, f.stream1)
	if a.MergeCommit != "upstream-merge" || a.MergeReference != p.Upstream+"#101" || a.CleanedAt != before.CleanedAt || a.ArchivedAt != before.ArchivedAt || a.Publication != before.Publication {
		t.Fatalf("archive after recovery=%+v", a)
	}
	check(false) // The sink's unmerged ready PR still keeps the shared spec active.
	sink := publications[1]
	host.Mu.Lock()
	host.Prs = append(host.Prs, pulls.PullRequest{Number: sink.PullRequest, State: "closed", Merged: true, MergeCommit: "sink-merge", Head: sink.Branch, HeadRepository: sink.Fork})
	host.Mu.Unlock()
	lifecycleAwait(t, f, "last member observed merged and group retired", func() bool {
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
		must(t, err)
		defer authority.Close()
		_, rev, err := trace.ResolvedWorkgroups(authority)
		must(t, err)
		return rev == 1
	})
	check(true)
	f.stop(t)
	f.start(t)
	check(true)
	down, err = f.s.repository(f.project2)
	must(t, err)
	after, afterRevision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if afterRevision != revision || !slices.Equal(after.Edges, obs.Edges) {
		t.Fatal("restart duplicated merge release")
	}
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	must(t, err)
	if !gone {
		t.Fatal("resolution recreated cleaned member history")
	}
}

func TestSameRepoArchivedMergeReceiptRequiresExactPublication(t *testing.T) {
	for _, name := range []string{"current", "cleaned current", "earlier PR", "unmerged", "no commit", "edge removed after observation"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, _, refresh := ownPRFixture(t)
			upstream := stream
			if name == "cleaned current" {
				upstream = config.WorkstreamID("w_00000000000000000000000000000881")
			}
			g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{upstream, quiet}, demoStart, ownerActor, "fixture")
			must(t, err)
			graph := []trace.Edge{{From: upstream, To: quiet, Type: trace.EdgeHardBlock}}
			if name == "edge removed after observation" {
				graph = nil
			}
			must(t, trace.RecordProjectSpec(ctx, repo, g, trace.ProjectSpec{Behavior: "Retained group contract", Graph: graph, Slices: []trace.Slice{{Workstream: upstream, Spec: "Upstream"}, {Workstream: quiet, Spec: "Survivor"}}}, demoStart, ownerActor, "fixture"))
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "cancelled", Revision: 1, Project: repo.Project(), Workstream: quiet, At: demoStart, Actor: ownerActor, Cause: "fixture"}
			_, err = repo.SetFeatureState(ctx, h, AbandonedState, "Survivor cancelled")
			must(t, err)
			p := deliverWithPullRequest(t, repo, cfg, 42)
			known := []config.WorkstreamID{stream, quiet}
			if upstream != stream {
				known = append(known, upstream)
			}
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: known}})
			must(t, err)
			defer store.Close()
			refresh.s.store = store
			a := runtime.Archive{Project: cfg.Project.ID, Workstream: upstream, Title: "Archived upstream", State: DeliveredState, ArchivedAt: demoStart, CleanupStartedAt: demoStart.Add(time.Minute), CleanedAt: demoStart.Add(2 * time.Minute), ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}}
			must(t, store.SetArchived(a))
			// This focused resolver fixture uses the durable post-cleanup archive shape;
			// the connected production test above exercises authorized deletion itself.
			status := edgeMergeStatus{Upstream: upstream, Type: trace.EdgeHardBlock, Merged: true, Reference: p.Upstream + "#42", Commit: "merge-tip"}
			if name == "earlier PR" {
				status.Reference = p.Upstream + "#41"
			}
			if name == "unmerged" {
				status.Merged = false
			}
			if name == "no commit" {
				status.Commit = ""
			}
			resolutionDocument(t, repo, quiet, edgeObservationDocument, edgeObservationPath, edgeObservation{Edges: []edgeMergeStatus{status}})
			resolver := &workgroupResolver{s: refresh.s, repository: repo}
			for range 2 {
				must(t, resolver.Pass(ctx))
			}
			got, _ := archiveRecord(t, store, upstream)
			ids, rev, err := trace.ResolvedWorkgroups(repo)
			must(t, err)
			active, err := trace.ProjectSpecActive(repo, g.ID)
			must(t, err)
			want := name == "current" || name == "cleaned current" || name == "no commit" || name == "edge removed after observation"
			if archiveResolved(got) != want || (len(ids) == 1) != want || (rev == 1) != want || active == want {
				t.Fatalf("archive=%+v groups=%v rev=%d active=%v", got, ids, rev, active)
			}
		})
	}
}
