package lifecycle_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDesignProductionPostCleanupMergeRetiresProjectSpecAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &fixturekit.FakePulls{}
	var interrupt, reached atomic.Bool
	interrupt.Store(true)
	sinkArchive := make(chan struct{})
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
		if step == "auto-archive-selected" {
			select {
			case <-sinkArchive:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
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
	group := resolutionGroupID(t, f)
	check := func(want bool) {
		t.Helper()
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
		fixturekit.Must(t, err)
		defer authority.Close()
		ids, rev, err := trace.ResolvedWorkgroups(authority)
		fixturekit.Must(t, err)
		active, err := trace.ProjectSpecActive(authority, group)
		fixturekit.Must(t, err)
		if want {
			if !slices.Equal(ids, []config.WorkgroupID{group}) || rev != 1 || active {
				t.Fatalf("group=%v rev=%d active=%v", ids, rev, active)
			}
		} else if len(ids) != 0 || rev != 0 || !active {
			t.Fatal("project spec retired before every member merged")
		}
		for _, role := range []string{intake.MasonRole, trace.ChiefOfStaff} {
			b, err := f.s.Context().Assemble(ctx, f.project2, bundle.Scope{Role: role, Workstream: f.stream2})
			fixturekit.Must(t, err)
			if (b.ProjectSpec != nil) == want {
				t.Fatalf("role %s spec presence=%v resolved=%v", role, b.ProjectSpec != nil, want)
			}
		}
		spec, found, err := trace.ProjectSpecFor(authority, group)
		fixturekit.Must(t, err)
		if !found || spec.Behavior == "" {
			t.Fatal("shared spec history lost")
		}
	}
	_, err := f.c.Archive(ctx, f.stream1)
	fixturekit.Must(t, err)
	lifecycleAwait(t, f, "unmerged upstream cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	before, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
	if !ok || before.Resolved || before.ResolutionEvidence != "unresolved" || before.Publication.PullRequest != publications[0].PullRequest {
		t.Fatalf("archive before merge=%+v", before)
	}
	check(false)
	p := publications[0]
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: "upstream-merge", Head: p.Branch, HeadRepository: p.Fork}}
	host.Mu.Unlock()
	lifecycleAwait(t, f, "durable merge receipt before archive reconciliation", reached.Load)
	down, err := f.s.repository(f.project2)
	fixturekit.Must(t, err)
	obs, revision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	fixturekit.Must(t, err)
	if revision == 0 || len(obs.Edges) != 1 || !obs.Edges[0].Merged || obs.Edges[0].Commit != "upstream-merge" {
		t.Fatalf("receipt=%+v rev=%d", obs, revision)
	}
	a, _ := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
	if a.Resolved {
		t.Fatal("interruption did not precede archive reconciliation")
	}
	check(false)
	f.stop(t)
	interrupt.Store(false)
	host.Mu.Lock()
	host.Prs = nil
	host.Mu.Unlock()
	f.start(t)
	lifecycleAwait(t, f, "recovered archived resolution", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
		return ok && lifecycle.ArchiveResolved(a)
	})
	a, _ = intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
	if a.MergeCommit != "upstream-merge" || a.MergeReference != p.Upstream+"#101" || a.CleanedAt != before.CleanedAt || a.ArchivedAt != before.ArchivedAt || a.Publication != before.Publication {
		t.Fatalf("archive after recovery=%+v", a)
	}
	check(false)
	sink := publications[1]
	host.Mu.Lock()
	host.Prs = append(host.Prs, pulls.PullRequest{Number: sink.PullRequest, State: "closed", Merged: true, MergeCommit: "sink-merge", Head: sink.Branch, HeadRepository: sink.Fork})
	host.Mu.Unlock()
	lifecycleAwait(t, f, "last member observed merged and group retired", func() bool {
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
		fixturekit.Must(t, err)
		defer authority.Close()
		_, rev, err := trace.ResolvedWorkgroups(authority)
		fixturekit.Must(t, err)
		return rev == 1
	})
	check(true)
	f.stop(t)
	f.start(t)
	check(true)
	down, err = f.s.repository(f.project2)
	fixturekit.Must(t, err)
	after, afterRevision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	fixturekit.Must(t, err)
	if afterRevision != revision || !slices.Equal(after.Edges, obs.Edges) {
		t.Fatal("restart duplicated merge release")
	}
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("resolution recreated cleaned member history")
	}
	close(sinkArchive)
	lifecycleAwait(t, f, "sink archive cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream2)
		return ok && !a.CleanedAt.IsZero()
	})
	if a, _ := intakefixture.ArchiveRecord(t, f.s.store, f.stream2); !lifecycle.ArchiveResolved(a) || a.MergeCommit != "sink-merge" || a.Publication.PullRequest != sink.PullRequest {
		t.Fatalf("sink archive=%+v", a)
	}
	authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
	fixturekit.Must(t, err)
	defer authority.Close()
	ids, rev, err := trace.ResolvedWorkgroups(authority)
	fixturekit.Must(t, err)
	active, err := trace.ProjectSpecActive(authority, group)
	fixturekit.Must(t, err)
	if !slices.Equal(ids, []config.WorkgroupID{group}) || rev != 1 || active {
		t.Fatalf("group after sink cleanup=%v rev=%d active=%v", ids, rev, active)
	}
}
func TestSameRepoArchivedMergeReceiptRequiresExactPublication(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"current", "cleaned current", "earlier PR", "unmerged", "no commit", "edge removed after observation"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, _, refresh := ownPRFixture(t)
			upstream := fixturekit.Stream
			if name == "cleaned current" {
				upstream = config.WorkstreamID("w_00000000000000000000000000000881")
			}
			g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{upstream, fixturekit.Quiet}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
			fixturekit.Must(t, err)
			graph := []trace.Edge{{From: upstream, To: fixturekit.Quiet, Type: trace.EdgeHardBlock}}
			if name == "edge removed after observation" {
				graph = nil
			}
			fixturekit.Must(t, trace.RecordProjectSpec(ctx, repo, g, trace.ProjectSpec{Behavior: "Retained group contract", Graph: graph, Slices: []trace.Slice{{Workstream: upstream, Spec: "Upstream"}, {Workstream: fixturekit.Quiet, Spec: "Survivor"}}}, fixturekit.DemoStart, intake.OwnerActor, "fixture"))
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "cancelled", Revision: 1, Project: repo.Project(), Workstream: fixturekit.Quiet, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "fixture"}
			_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "Survivor cancelled")
			fixturekit.Must(t, err)
			p := deliverWithPullRequest(t, repo, cfg, 42)
			known := []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet}
			if upstream != fixturekit.Stream {
				known = append(known, upstream)
			}
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: known}})
			fixturekit.Must(t, err)
			defer store.Close()
			refresh.s.store = store
			a := runtime.Archive{Project: cfg.Project.ID, Workstream: upstream, Title: "Archived upstream", State: intake.DeliveredState, ArchivedAt: fixturekit.DemoStart, CleanupStartedAt: fixturekit.DemoStart.Add(time.Minute), CleanedAt: fixturekit.DemoStart.Add(2 * time.Minute), ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}}
			fixturekit.Must(t, store.SetArchived(a))
			status := lifecycle.EdgeMergeStatus{Upstream: upstream, Type: trace.EdgeHardBlock, Merged: true, Reference: p.Upstream + "#42", Commit: "merge-tip"}
			if name == "earlier PR" {
				status.Reference = p.Upstream + "#41"
			}
			if name == "unmerged" {
				status.Merged = false
			}
			if name == "no commit" {
				status.Commit = ""
			}
			resolutionDocument(t, repo, fixturekit.Quiet, lifecycle.EdgeObservationDocument, lifecycle.EdgeObservationPath, lifecycle.EdgeObservation{Edges: []lifecycle.EdgeMergeStatus{status}})
			resolver := &workgroupResolver{s: refresh.s, repository: repo}
			for range 2 {
				fixturekit.Must(t, resolver.Pass(ctx))
			}
			got, _ := intakefixture.ArchiveRecord(t, store, upstream)
			ids, rev, err := trace.ResolvedWorkgroups(repo)
			fixturekit.Must(t, err)
			active, err := trace.ProjectSpecActive(repo, g.ID)
			fixturekit.Must(t, err)
			want := name == "current" || name == "cleaned current" || name == "no commit" || name == "edge removed after observation"
			if lifecycle.ArchiveResolved(got) != want || (len(ids) == 1) != want || (rev == 1) != want || active == want {
				t.Fatalf("archive=%+v groups=%v rev=%d active=%v", got, ids, rev, active)
			}
		})
	}
}
