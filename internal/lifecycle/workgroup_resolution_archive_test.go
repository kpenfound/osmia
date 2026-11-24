package lifecycle_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDesignProductionArchiveRequiresPinnedMergeEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ready-unmerged", "owner-merged", "own-pr-merged"} {
		merged := name != "ready-unmerged"
		t.Run(name, func(t *testing.T) {
			gate := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
			t.Cleanup(gate.Release)
			f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: gate.Checkpoint}, "contract", nil)
			defer f.Stop(t)
			deliverResolutionMembers(t, f)
			second, err := f.S.Repository(f.Project2)
			fixturekit.Must(t, err)
			journey.RecordResolutionMerge(t, second, f.Stream2)
			first, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			if name == "owner-merged" {
				journey.RecordResolutionMerge(t, first, f.Stream1)
			} else if name == "own-pr-merged" {
				journey.ResolutionDocument(t, first, f.Stream1, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: 101, Outcome: delivery.OwnPullRequestOutcomeMerged, MergeCommit: "merged-upstream"})
			}
			_, err = f.C.Archive(context.Background(), f.Stream1)
			fixturekit.Must(t, err)
			gate.Await(t)
			checkArchive := func(boundary string) {
				t.Helper()
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
				if !ok || a.Resolved != merged || lifecycle.ArchiveResolved(a) != merged {
					t.Fatalf("%s archive=%+v", boundary, a)
				}
				view, err := f.C.Status(context.Background(), f.Stream1)
				fixturekit.Must(t, err)
				if (view.DisplayState == "delivered (merged)") != merged {
					t.Fatalf("%s label=%s", boundary, view.DisplayState)
				}
				if !merged {
					resolutionGroupState(t, f, false)
					if !strings.Contains(view.Wait, "no pinned merge or cancel evidence") {
						t.Fatalf("%s missing truthful archive diagnostic: %+v", boundary, view)
					}
				}
			}
			checkArchive("retained")
			if merged {
				awaitMemberMergeResolution(t, f)
			}
			gate.Release()
			journey.LifecycleAwait(t, f, "archive cleanup", func() bool {
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
				return ok && !a.CleanedAt.IsZero()
			})
			checkDeleted := func() {
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
				if !ok {
					t.Fatal("archive disappeared")
				}
				if lifecycle.ArchiveResolved(a) != merged {
					t.Fatalf("deleted evidence=%+v", a)
				}
				view, err := f.C.Status(context.Background(), f.Stream1)
				fixturekit.Must(t, err)
				if (view.DisplayState == "delivered (merged)") != merged {
					t.Fatalf("deleted label=%s", view.DisplayState)
				}
				if !merged && !strings.Contains(view.Wait, "no pinned merge or cancel evidence") {
					t.Fatal("deleted member lost its resolution hold diagnostic")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, a.Workgroup)
				fixturekit.Must(t, err)
				defer authority.Close()
				ids, rev, err := trace.ResolvedWorkgroups(authority)
				fixturekit.Must(t, err)
				active, err := trace.ProjectSpecActive(authority, a.Workgroup)
				fixturekit.Must(t, err)
				if merged {
					if !slices.Equal(ids, []config.WorkgroupID{a.Workgroup}) || rev != 1 || active {
						t.Fatal("merged archived member did not resolve once")
					}
				} else if len(ids) != 0 || rev != 0 || !active {
					t.Fatal("retirement granted resolution")
				}
				gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
				fixturekit.Must(t, err)
				if !gone {
					t.Fatal("deleted member history was recreated")
				}
			}
			checkDeleted()
			f.Stop(t)
			f.Start(t)
			checkDeleted()
		})
	}
}
func TestSingleMemberResolutionAndDeliveryCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, member, repo := newCleanupFixture(t, config.WorkspacesGit)
	defer repo.Close()
	for _, unit := range []string{"upload", "audit"} {
		journey.CompleteMasonTurn(t, f, repo, member, unit, "built "+unit)
	}
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{member}, f.S.Now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	if len(groups) == 0 {
		t.Fatal("missing one-member group")
	}
	p := api.DeliveryPublication{Status: delivery.PublicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"}
	journey.ResolutionDocument(t, repo, member, delivery.PublicationDocument, delivery.PublicationPath, p)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ready-delivered", Revision: 1, Project: repo.Project(), Workstream: member, At: f.S.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "ready PR published")
	fixturekit.Must(t, err)
	resolver := &journey.WorkgroupResolver{S: f.S, Repository: repo}
	fixturekit.Must(t, resolver.Pass(ctx))
	ids, revision, err := trace.ResolvedWorkgroups(repo)
	fixturekit.Must(t, err)
	if len(ids) != 0 || revision != 0 {
		t.Fatal("one-member ready PR resolved")
	}
	state := intake.DeliveredState
	view := api.WorkstreamStatus{Project: repo.Project(), Workstream: member, State: &state}
	fixturekit.Must(t, journey.InspectForTest(f.S).PresentWorkstream(repo, &view))
	if view.DisplayState != "PR open" {
		t.Fatalf("live label=%s", view.DisplayState)
	}
	cleanup := newCleanup(f, repo)
	fixturekit.Must(t, cleanup.Pass(ctx))
	if len(cleanup.Failures) != 0 {
		t.Fatalf("delivery cleanup=%v", cleanup.Failures)
	}
	fixturekit.AssertGone(t, filepath.Join(f.S.Cfg.Root.String(), intake.BranchesDirectory, string(f.Project), string(member)))
	for _, unit := range []string{"upload", "audit"} {
		assertUnitWorkspace(t, f, repo, member, unit, false)
	}
	journey.RecordResolutionMerge(t, repo, member)
	fixturekit.Must(t, resolver.Pass(ctx))
	fixturekit.Must(t, resolver.Pass(ctx))
	ids, revision, err = trace.ResolvedWorkgroups(repo)
	fixturekit.Must(t, err)
	if len(ids) != 1 || revision != 1 {
		t.Fatalf("one-member merge resolution=%v revision=%d", ids, revision)
	}
}
func TestSingleMemberReadyArchivePinsUnresolved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := journey.ProjectFixture(t)
	gate := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(gate.Release)
	opts.Boundary = gate.Checkpoint
	s, c := journey.StartT(t, opts)
	defer func() { fixturekit.Must(t, s.Close()) }()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	repo := s.Sole().Repository
	member := config.WorkstreamID("w_00000000000000000000000000000991")
	fixturekit.Must(t, repo.CreateWorkstream(ctx, member, s.Now(), intake.OwnerActor))
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, s.Now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	journey.ResolutionDocument(t, repo, member, delivery.PublicationDocument, delivery.PublicationPath, api.DeliveryPublication{Status: delivery.PublicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: s.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "ready PR published")
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.RefreshRuntime())
	_, err = c.Archive(ctx, member)
	fixturekit.Must(t, err)
	gate.Await(t)
	a, ok := intakefixture.ArchiveRecord(t, s.Store, member)
	if !ok || a.Resolved || a.ResolutionEvidence != "unresolved" {
		t.Fatalf("one-member archive=%+v", a)
	}
	view, err := c.Status(ctx, member)
	fixturekit.Must(t, err)
	if view.DisplayState != "blocked" || !strings.Contains(view.Wait, "no pinned merge or cancel evidence") {
		t.Fatalf("archived label=%s", view.DisplayState)
	}
	ids, _, err := trace.ResolvedWorkgroups(repo)
	fixturekit.Must(t, err)
	if slices.Contains(ids, g.ID) {
		t.Fatal("one-member archive resolved an unmerged PR")
	}
	gate.Release()
	changes, closeChanges := journey.ServiceChanges(t, s)
	defer closeChanges()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		a, _ = intakefixture.ArchiveRecord(t, s.Store, member)
		if !a.CleanedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not complete")
		}
		changes()
	}
	ids, _, err = trace.ResolvedWorkgroups(repo)
	fixturekit.Must(t, err)
	if slices.Contains(ids, g.ID) {
		t.Fatal("one-member cleanup resolved an unmerged PR")
	}
}
