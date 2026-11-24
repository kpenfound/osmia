package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

func TestDesignProductionArchivedOwnPullRequestsWithoutObservers(t *testing.T) {
	t.Parallel()
	for _, edge := range []string{trace.EdgeHardBlock, trace.EdgeContract, ""} {
		t.Run("edge-"+edge, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			host := &fixturekit.FakePulls{}
			var interrupt, reached atomic.Bool
			f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
				if step == "archive-own-pr-merge-observed" && interrupt.Load() {
					reached.Store(true)
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}, edge, host)
			defer f.Stop(t)
			publications := deliverResolutionMembers(t, f)
			repo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			group, err := repo.CrossRepoWorkgroupOf(f.Stream1)
			fixturekit.Must(t, err)
			checkGroup := func(resolved bool) {
				t.Helper()
				authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
				fixturekit.Must(t, err)
				defer authority.Close()
				ids, rev, err := trace.ResolvedWorkgroups(authority)
				fixturekit.Must(t, err)
				active, err := trace.ProjectSpecActive(authority, group)
				fixturekit.Must(t, err)
				if resolved {
					if !slices.Equal(ids, []config.WorkgroupID{group}) || rev != 1 || active {
						t.Fatalf("groups=%v rev=%d active=%v", ids, rev, active)
					}
				} else if len(ids) != 0 || rev != 0 || !active {
					t.Fatalf("premature retirement: groups=%v rev=%d active=%v", ids, rev, active)
				}
				_, found, err := trace.ProjectSpecFor(authority, group)
				fixturekit.Must(t, err)
				if !found {
					t.Fatal("project spec history lost")
				}
			}
			merge := func(i int) {
				host.Mu.Lock()
				defer host.Mu.Unlock()
				p := publications[i]
				host.Prs = append(host.Prs, pulls.PullRequest{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: fmt.Sprintf("merged-%d", i), Head: p.Branch, HeadRepository: p.Fork})
				f.Clock.Advance(10 * time.Minute)
			}
			awaitFirstMerge := func() {
				journey.LifecycleAwait(t, f, "first member merge observed", func() bool {
					if a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1); ok {
						return lifecycle.ArchiveResolved(a)
					}
					out, rev, err := delivery.OwnPullRequestOutcomeAt(repo, f.Stream1)
					fixturekit.Must(t, err)
					return rev > 0 && out.Outcome == "merged"
				})
			}
			if edge == trace.EdgeHardBlock {
				merge(0)
				awaitFirstMerge()
				down, err := f.S.Repository(f.Project2)
				fixturekit.Must(t, err)
				journey.LifecycleAwait(t, f, "upstream wait cleared before sink archive", func() bool {
					waiting, err := lifecycle.EdgeWaitDisplay(down, f.Stream2)
					fixturekit.Must(t, err)
					return waiting == ""
				})
			}
			archived := resolutionMembers(f)
			if edge == trace.EdgeHardBlock {
				archived = archived[1:]
			}
			before := map[config.WorkstreamID]runtime.Archive{}
			for _, m := range archived {
				_, err := f.C.Archive(ctx, m.stream)
				fixturekit.Must(t, err)
				journey.LifecycleAwait(t, f, "unmerged member cleanup", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
					return ok && !a.CleanedAt.IsZero()
				})
				a, _ := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
				if lifecycle.ArchiveResolved(a) || a.Publication.PullRequest <= 0 {
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
			journey.LifecycleAwait(t, f, "exact archived sink merge before persistence", reached.Load)
			a, _ := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream2)
			if lifecycle.ArchiveResolved(a) {
				t.Fatal("interruption did not precede archive merge persistence")
			}
			checkGroup(false)
			f.Stop(t)
			interrupt.Store(false)
			f.Start(t)
			journey.LifecycleAwait(t, f, "all archived publications resolved after restart", func() bool {
				for _, m := range archived {
					a, ok := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
					if !ok || !lifecycle.ArchiveResolved(a) {
						return false
					}
				}
				authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
				fixturekit.Must(t, err)
				defer authority.Close()
				_, rev, err := trace.ResolvedWorkgroups(authority)
				fixturekit.Must(t, err)
				return rev == 1
			})
			checkGroup(true)
			f.Stop(t)
			host.Mu.Lock()
			calls := host.Finds
			host.Mu.Unlock()
			f.Start(t)
			checkGroup(true)
			for _, m := range archived {
				a, _ := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
				old := before[m.stream]
				if !lifecycle.ArchiveResolved(a) || a.ResolutionEvidence != "merged" || a.MergeCommit == "" || a.MergeReference != fmt.Sprintf("%s#%d", old.Publication.Upstream, old.Publication.PullRequest) || a.Publication != old.Publication || a.ArchivedAt != old.ArchivedAt || a.CleanedAt != old.CleanedAt {
					t.Fatalf("archive identity or evidence changed: %+v", a)
				}
				gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, m.project, m.stream)
				fixturekit.Must(t, err)
				if !gone {
					t.Fatal("archived observation recreated deleted history")
				}
			}
			host.Mu.Lock()
			after := host.Finds
			host.Mu.Unlock()
			if after != calls {
				t.Fatalf("merged archives polled again: %d -> %d", calls, after)
			}
		})
	}
}
func TestArchivedOwnPullRequestCadenceAndExactIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, host, refresh := journey.OwnPRFixture(t)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	refresh.S.Options.Reconciliation.Now = clock.Now
	p := journey.DeliverWithPullRequest(t, repo, cfg, 42)
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{fixturekit.Stream}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
	fixturekit.Must(t, err)
	defer store.Close()
	refresh.S.Store = store
	a := runtime.Archive{Project: cfg.Project.ID, Workstream: fixturekit.Stream, State: intake.DeliveredState, ArchivedAt: fixturekit.DemoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}}
	fixturekit.Must(t, store.SetArchived(a))
	docs, err := trace.Read[trace.Document](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	for i, scenario := range []string{"failure", "earlier PR", "open", "closed", "missing", "merged"} {
		host.FindErr = nil
		host.Prs = nil
		pr := pulls.PullRequest{Number: 42, Head: p.Branch, HeadRepository: p.Fork}
		switch scenario {
		case "failure":
			host.FindErr = errors.New("provider unavailable")
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
			host.Prs = []pulls.PullRequest{pr}
		}
		fixturekit.Must(t, refresh.Pass(ctx))
		got, _ := intakefixture.ArchiveRecord(t, store, fixturekit.Stream)
		if lifecycle.ArchiveResolved(got) != (scenario == "merged") || host.Finds != i+1 {
			t.Fatalf("%s: archive=%+v finds=%d", scenario, got, host.Finds)
		}
		clock.Advance(10*time.Minute - time.Nanosecond)
		fixturekit.Must(t, refresh.Pass(ctx))
		if host.Finds != i+1 {
			t.Fatalf("%s retried before cadence", scenario)
		}
		clock.Advance(time.Nanosecond)
	}
	fixturekit.Must(t, (&journey.WorkgroupResolver{S: refresh.S, Repository: repo}).Pass(ctx))
	ids, rev, err := trace.ResolvedWorkgroups(repo)
	fixturekit.Must(t, err)
	if !slices.Equal(ids, []config.WorkgroupID{g.ID}) || rev != 1 {
		t.Fatalf("single member group=%v rev=%d", ids, rev)
	}
	after, err := trace.Read[trace.Document](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !slices.Equal(docs, after) {
		t.Fatal("archived observation mutated retained member history")
	}
}
func TestSingleMemberArchivedOwnPullRequestAfterCleanupAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := journey.ProjectFixture(t)
	host := &fixturekit.FakePulls{}
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts.PullRequests = host
	opts.Reconciliation.Now = clock.Now
	s, c := journey.StartT(t, opts)
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	repo := s.Sole().Repository
	member := config.WorkstreamID("w_00000000000000000000000000000891")
	fixturekit.Must(t, repo.CreateWorkstream(ctx, member, clock.Now(), intake.OwnerActor))
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, clock.Now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "dagger/dagger", Fork: "owner/dagger", Branch: intake.FeatureBranch(member), PullRequest: 42, URL: "https://example.invalid/pull/42"}
	journey.ResolutionDocument(t, repo, member, delivery.PublicationDocument, delivery.PublicationPath, p)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "Owner-approved ready PR")
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.RefreshRuntime())
	_, err = c.Archive(ctx, member)
	fixturekit.Must(t, err)
	await := func(phase string, ready func() bool) {
		t.Helper()
		changes, closeChanges := journey.ServiceChanges(t, s)
		defer closeChanges()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", phase)
			}
			clock.Advance(10 * time.Minute)
			changes()
		}
	}
	await("single member cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, s.Store, member)
		return ok && !a.CleanedAt.IsZero()
	})
	before, _ := intakefixture.ArchiveRecord(t, s.Store, member)
	if lifecycle.ArchiveResolved(before) {
		t.Fatal("archive or cleanup supplied a merge")
	}
	fixturekit.Must(t, s.Close())
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, MergeCommit: "single-merge", State: "closed", Head: p.Branch, HeadRepository: p.Fork}}
	host.Mu.Unlock()
	s, c = journey.StartT(t, opts)
	await("single archived merge and resolution", func() bool {
		a, _ := intakefixture.ArchiveRecord(t, s.Store, member)
		ids, rev, err := trace.ResolvedWorkgroups(s.Sole().Repository)
		fixturekit.Must(t, err)
		return lifecycle.ArchiveResolved(a) && rev == 1 && slices.Equal(ids, []config.WorkgroupID{g.ID})
	})
	view, err := c.Status(ctx, member)
	fixturekit.Must(t, err)
	if view.DisplayState != "delivered (merged)" || view.Wait != "" {
		t.Fatalf("single archived status=%+v", view)
	}
	fixturekit.Must(t, s.Close())
	s, _ = journey.StartT(t, opts)
	ids, rev, err := trace.ResolvedWorkgroups(s.Sole().Repository)
	fixturekit.Must(t, err)
	a, _ := intakefixture.ArchiveRecord(t, s.Store, member)
	gone, err := trace.WorkstreamTraceGone(s.Current().Root, added.Project.ID, member)
	fixturekit.Must(t, err)
	if !gone || !lifecycle.ArchiveResolved(a) || a.MergeCommit != "single-merge" || a.Publication != before.Publication || a.CleanedAt != before.CleanedAt || !slices.Equal(ids, []config.WorkgroupID{g.ID}) || rev != 1 {
		t.Fatalf("single member recovery: gone=%v archive=%+v groups=%v revision=%d", gone, a, ids, rev)
	}
}
