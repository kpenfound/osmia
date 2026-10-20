package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDesignProductionArchivedOwnPullRequestsWithoutObservers(t *testing.T) {
	for _, edge := range []string{trace.EdgeHardBlock, trace.EdgeContract, ""} {
		t.Run("edge-"+edge, func(t *testing.T) {
			ctx := context.Background()
			host := &fakePulls{}
			var interrupt, reached atomic.Bool
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
				if step == "archive-own-pr-merge-observed" && interrupt.Load() {
					reached.Store(true)
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}, edge, host)
			defer f.stop(t)
			publications := deliverResolutionMembers(t, f)
			repo, err := f.s.repository(f.project)
			must(t, err)
			group, err := repo.CrossRepoWorkgroupOf(f.stream1)
			must(t, err)
			checkGroup := func(resolved bool) {
				t.Helper()
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
				must(t, err)
				defer authority.Close()
				ids, rev, err := trace.ResolvedWorkgroups(authority)
				must(t, err)
				active, err := trace.ProjectSpecActive(authority, group)
				must(t, err)
				if resolved {
					if !slices.Equal(ids, []config.WorkgroupID{group}) || rev != 1 || active {
						t.Fatalf("groups=%v rev=%d active=%v", ids, rev, active)
					}
				} else if len(ids) != 0 || rev != 0 || !active {
					t.Fatalf("premature retirement: groups=%v rev=%d active=%v", ids, rev, active)
				}
				_, found, err := trace.ProjectSpecFor(authority, group)
				must(t, err)
				if !found {
					t.Fatal("project spec history lost")
				}
			}
			merge := func(i int) {
				host.mu.Lock()
				defer host.mu.Unlock()
				p := publications[i]
				host.prs = append(host.prs, pulls.PullRequest{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: fmt.Sprintf("merged-%d", i), Head: p.Branch, HeadRepository: p.Fork})
				f.clock.Advance(10 * time.Minute)
			}
			awaitFirstMerge := func() {
				lifecycleAwait(t, f, "first member merge observed", func() bool {
					if a, ok := archiveRecord(t, f.s.store, f.stream1); ok {
						return archiveResolved(a)
					}
					out, rev, err := ownPullRequestOutcomeAt(repo, f.stream1)
					must(t, err)
					return rev > 0 && out.Outcome == "merged"
				})
			}
			if edge == trace.EdgeHardBlock {
				// The sink's workspace is eligible for cleanup only after its
				// upstream wait clears through the ordinary edge observer.
				merge(0)
				awaitFirstMerge()
				down, err := f.s.repository(f.project2)
				must(t, err)
				lifecycleAwait(t, f, "upstream wait cleared before sink archive", func() bool {
					waiting, err := edgeWaitDisplay(down, f.stream2)
					must(t, err)
					return waiting == ""
				})
			}
			// A hard-block sink has no downstream observer. The other graphs have
			// no merge observers at all, including when every member is archived.
			archived := resolutionMembers(f)
			if edge == trace.EdgeHardBlock {
				archived = archived[1:]
			}
			before := map[config.WorkstreamID]runtime.Archive{}
			for _, m := range archived {
				_, err := f.c.Archive(ctx, m.stream)
				must(t, err)
				lifecycleAwait(t, f, "unmerged member cleanup", func() bool {
					a, ok := archiveRecord(t, f.s.store, m.stream)
					return ok && !a.CleanedAt.IsZero()
				})
				a, _ := archiveRecord(t, f.s.store, m.stream)
				if archiveResolved(a) || a.Publication.PullRequest <= 0 {
					t.Fatalf("unmerged archive=%+v", a)
				}
				before[m.stream] = a
			}
			checkGroup(false)
			if edge != trace.EdgeHardBlock {
				merge(0)
				awaitFirstMerge()
			}
			checkGroup(false)
			interrupt.Store(true)
			merge(1)
			lifecycleAwait(t, f, "exact archived sink merge before persistence", reached.Load)
			a, _ := archiveRecord(t, f.s.store, f.stream2)
			if archiveResolved(a) {
				t.Fatal("interruption did not precede archive merge persistence")
			}
			checkGroup(false)
			f.stop(t)
			interrupt.Store(false)
			f.start(t)
			lifecycleAwait(t, f, "all archived publications resolved after restart", func() bool {
				for _, m := range archived {
					a, ok := archiveRecord(t, f.s.store, m.stream)
					if !ok || !archiveResolved(a) {
						return false
					}
				}
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
				must(t, err)
				defer authority.Close()
				_, rev, err := trace.ResolvedWorkgroups(authority)
				must(t, err)
				return rev == 1
			})
			checkGroup(true)
			f.stop(t)
			host.mu.Lock()
			calls := host.finds
			host.mu.Unlock()
			f.start(t)
			checkGroup(true)
			for _, m := range archived {
				a, _ := archiveRecord(t, f.s.store, m.stream)
				old := before[m.stream]
				if !archiveResolved(a) || a.ResolutionEvidence != "merged" || a.MergeCommit == "" || a.MergeReference != fmt.Sprintf("%s#%d", old.Publication.Upstream, old.Publication.PullRequest) || a.Publication != old.Publication || a.ArchivedAt != old.ArchivedAt || a.CleanedAt != old.CleanedAt {
					t.Fatalf("archive identity or evidence changed: %+v", a)
				}
				gone, err := trace.WorkstreamTraceGone(f.s.current().Root, m.project, m.stream)
				must(t, err)
				if !gone {
					t.Fatal("archived observation recreated deleted history")
				}
			}
			host.mu.Lock()
			after := host.finds
			host.mu.Unlock()
			if after != calls {
				t.Fatalf("merged archives polled again: %d -> %d", calls, after)
			}
		})
	}
}

func TestArchivedOwnPullRequestCadenceAndExactIdentity(t *testing.T) {
	ctx := context.Background()
	cfg, repo, host, refresh := ownPRFixture(t)
	clock := &fixedClock{now: demoStart}
	refresh.s.options.Reconciliation.Now = clock.Now
	p := deliverWithPullRequest(t, repo, cfg, 42)
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{stream}, demoStart, ownerActor, "fixture")
	must(t, err)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
	must(t, err)
	defer store.Close()
	refresh.s.store = store
	a := runtime.Archive{Project: cfg.Project.ID, Workstream: stream, State: DeliveredState, ArchivedAt: demoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}}
	must(t, store.SetArchived(a))
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	for i, scenario := range []string{"failure", "earlier PR", "open", "closed", "missing", "merged"} {
		host.findErr = nil
		host.prs = nil
		pr := pulls.PullRequest{Number: 42, Head: p.Branch, HeadRepository: p.Fork}
		switch scenario {
		case "failure":
			host.findErr = errors.New("provider unavailable")
		case "earlier PR":
			pr.Number, pr.Merged = 41, true
		case "open":
			pr.State = "open"
		case "closed":
			pr.State = "closed"
		case "merged":
			pr.Merged, pr.MergeCommit = true, "merged-tip"
		}
		if scenario != "missing" {
			host.prs = []pulls.PullRequest{pr}
		}
		must(t, refresh.Pass(ctx))
		got, _ := archiveRecord(t, store, stream)
		if archiveResolved(got) != (scenario == "merged") || host.finds != i+1 {
			t.Fatalf("%s: archive=%+v finds=%d", scenario, got, host.finds)
		}
		clock.Advance(10*time.Minute - time.Nanosecond)
		must(t, refresh.Pass(ctx))
		if host.finds != i+1 {
			t.Fatalf("%s retried before cadence", scenario)
		}
		clock.Advance(time.Nanosecond)
	}
	must(t, (&workgroupResolver{s: refresh.s, repository: repo}).Pass(ctx))
	ids, rev, err := trace.ResolvedWorkgroups(repo)
	must(t, err)
	if !slices.Equal(ids, []config.WorkgroupID{g.ID}) || rev != 1 {
		t.Fatalf("single member group=%v rev=%d", ids, rev)
	}
	after, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	if !slices.Equal(docs, after) {
		t.Fatal("archived observation mutated retained member history")
	}
}

func TestSingleMemberArchivedOwnPullRequestAfterCleanupAndRestart(t *testing.T) {
	ctx := context.Background()
	opts, clone := projectFixture(t)
	host := &fakePulls{}
	clock := &fixedClock{now: demoStart}
	opts.PullRequests = host
	opts.Reconciliation.Now = clock.Now
	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	repo := s.sole().repository
	member := config.WorkstreamID("w_00000000000000000000000000000891")
	must(t, repo.CreateWorkstream(ctx, member, clock.Now(), ownerActor))
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, clock.Now(), ownerActor, "fixture")
	must(t, err)
	p := DeliveryPublication{Status: publicationOpened, Upstream: "dagger/dagger", Fork: "owner/dagger", Branch: featureBranch(member), PullRequest: 42, URL: "https://example.invalid/pull/42"}
	resolutionDocument(t, repo, member, publicationDocument, publicationPath, p)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: clock.Now(), Actor: ownerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "Owner-approved ready PR")
	must(t, err)
	must(t, s.refreshRuntime())
	_, err = c.Archive(ctx, member)
	must(t, err)
	await := func(phase string, ready func() bool) {
		t.Helper()
		changes, closeChanges := serviceChanges(t, s)
		defer closeChanges()
		deadline := time.Now().Add(demoTimeout)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", phase)
			}
			clock.Advance(10 * time.Minute)
			changes()
		}
	}
	await("single member cleanup", func() bool {
		a, ok := archiveRecord(t, s.store, member)
		return ok && !a.CleanedAt.IsZero()
	})
	before, _ := archiveRecord(t, s.store, member)
	if archiveResolved(before) {
		t.Fatal("archive or cleanup supplied a merge")
	}
	must(t, s.Close())
	host.mu.Lock()
	host.prs = []pulls.PullRequest{{Number: 42, Merged: true, MergeCommit: "single-merge", State: "closed", Head: p.Branch, HeadRepository: p.Fork}}
	host.mu.Unlock()
	s, c = start(t, opts)
	await("single archived merge and resolution", func() bool {
		a, _ := archiveRecord(t, s.store, member)
		ids, rev, err := trace.ResolvedWorkgroups(s.sole().repository)
		must(t, err)
		return archiveResolved(a) && rev == 1 && slices.Equal(ids, []config.WorkgroupID{g.ID})
	})
	view, err := c.Status(ctx, member)
	must(t, err)
	if view.DisplayState != "delivered (merged)" || view.Wait != "" {
		t.Fatalf("single archived status=%+v", view)
	}
	must(t, s.Close())
	s, _ = start(t, opts)
	ids, rev, err := trace.ResolvedWorkgroups(s.sole().repository)
	must(t, err)
	a, _ := archiveRecord(t, s.store, member)
	gone, err := trace.WorkstreamTraceGone(s.current().Root, added.Project.ID, member)
	must(t, err)
	if !gone || !archiveResolved(a) || a.MergeCommit != "single-merge" || a.Publication != before.Publication || a.CleanedAt != before.CleanedAt || !slices.Equal(ids, []config.WorkgroupID{g.ID}) || rev != 1 {
		t.Fatalf("single member recovery: gone=%v archive=%+v groups=%v revision=%d", gone, a, ids, rev)
	}
}
