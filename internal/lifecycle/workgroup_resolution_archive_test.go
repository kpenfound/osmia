package lifecycle_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func rewriteLegacyResolutionArchive(t *testing.T, f *productionDesignFixture, member config.WorkstreamID) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "runtime.json")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	var state map[string]any
	fixturekit.Must(t, json.Unmarshal(data, &state))
	for _, raw := range state["archived"].([]any) {
		a := raw.(map[string]any)
		if a["workstream"] == string(member) {
			a["resolved"] = true
			delete(a, "resolution_evidence")
		}
	}
	data, err = json.Marshal(state)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, data, 0600))
}
func TestDesignProductionArchiveRequiresPinnedMergeEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ready-unmerged", "owner-merged", "own-pr-merged"} {
		merged := name != "ready-unmerged"
		t.Run(name, func(t *testing.T) {
			gate := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
			t.Cleanup(gate.release)
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: gate.checkpoint}, "contract", nil)
			defer f.stop(t)
			deliverResolutionMembers(t, f)
			second, err := f.s.repository(f.project2)
			fixturekit.Must(t, err)
			recordResolutionMerge(t, second, f.stream2)
			first, err := f.s.repository(f.project)
			fixturekit.Must(t, err)
			if name == "owner-merged" {
				recordResolutionMerge(t, first, f.stream1)
			} else if name == "own-pr-merged" {
				resolutionDocument(t, first, f.stream1, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: 101, Outcome: delivery.OwnPullRequestOutcomeMerged, MergeCommit: "merged-upstream"})
			}
			_, err = f.c.Archive(context.Background(), f.stream1)
			fixturekit.Must(t, err)
			gate.await(t)
			checkArchive := func(boundary string) {
				t.Helper()
				a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
				if !ok || a.Resolved != merged || lifecycle.ArchiveResolved(a) != merged {
					t.Fatalf("%s archive=%+v", boundary, a)
				}
				view, err := f.c.Status(context.Background(), f.stream1)
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
			if !merged {
				f.stop(t)
				rewriteLegacyResolutionArchive(t, f, f.stream1)
				f.start(t)
				checkArchive("legacy retained re-derived")
			}
			if merged {
				awaitMemberMergeResolution(t, f)
			}
			gate.release()
			lifecycleAwait(t, f, "archive cleanup", func() bool {
				a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
				return ok && !a.CleanedAt.IsZero()
			})
			checkDeleted := func() {
				a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
				if !ok {
					t.Fatal("archive disappeared")
				}
				if lifecycle.ArchiveResolved(a) != merged {
					t.Fatalf("deleted evidence=%+v", a)
				}
				view, err := f.c.Status(context.Background(), f.stream1)
				fixturekit.Must(t, err)
				if (view.DisplayState == "delivered (merged)") != merged {
					t.Fatalf("deleted label=%s", view.DisplayState)
				}
				if !merged && !strings.Contains(view.Wait, "no pinned merge or cancel evidence") {
					t.Fatal("deleted member lost its resolution hold diagnostic")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, a.Workgroup)
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
					t.Fatal("retirement or legacy boolean granted resolution")
				}
				gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
				fixturekit.Must(t, err)
				if !gone {
					t.Fatal("deleted member history was recreated")
				}
			}
			checkDeleted()
			f.stop(t)
			if !merged {
				rewriteLegacyResolutionArchive(t, f, f.stream1)
			}
			f.start(t)
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
		completeMasonTurn(t, f, repo, member, unit, "built "+unit)
	}
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{member}, f.s.now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	if len(groups) == 0 {
		t.Fatal("missing one-member group")
	}
	p := api.DeliveryPublication{Status: delivery.PublicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"}
	resolutionDocument(t, repo, member, delivery.PublicationDocument, delivery.PublicationPath, p)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ready-delivered", Revision: 1, Project: repo.Project(), Workstream: member, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "ready PR published")
	fixturekit.Must(t, err)
	resolver := &workgroupResolver{s: f.s, repository: repo}
	fixturekit.Must(t, resolver.Pass(ctx))
	ids, revision, err := trace.ResolvedWorkgroups(repo)
	fixturekit.Must(t, err)
	if len(ids) != 0 || revision != 0 {
		t.Fatal("one-member ready PR resolved")
	}
	state := intake.DeliveredState
	view := api.WorkstreamStatus{Project: repo.Project(), Workstream: member, State: &state}
	fixturekit.Must(t, f.s.presentWorkstream(repo, &view))
	if view.DisplayState != "PR open" {
		t.Fatalf("live label=%s", view.DisplayState)
	}
	cleanup := newCleanup(f, repo)
	fixturekit.Must(t, cleanup.Pass(ctx))
	if len(cleanup.failures) != 0 {
		t.Fatalf("delivery cleanup=%v", cleanup.failures)
	}
	fixturekit.AssertGone(t, filepath.Join(f.s.cfg.Root.String(), intake.BranchesDirectory, string(f.project), string(member)))
	for _, unit := range []string{"upload", "audit"} {
		assertUnitWorkspace(t, f, repo, member, unit, false)
	}
	recordResolutionMerge(t, repo, member)
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
	opts, clone := projectFixture(t)
	gate := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(gate.release)
	opts.boundary = gate.checkpoint
	s, c := start(t, opts)
	defer func() { fixturekit.Must(t, s.Close()) }()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	repo := s.sole().repository
	member := config.WorkstreamID("w_00000000000000000000000000000991")
	fixturekit.Must(t, repo.CreateWorkstream(ctx, member, s.now(), intake.OwnerActor))
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, s.now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	resolutionDocument(t, repo, member, delivery.PublicationDocument, delivery.PublicationPath, api.DeliveryPublication{Status: delivery.PublicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "ready PR published")
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.refreshRuntime())
	_, err = c.Archive(ctx, member)
	fixturekit.Must(t, err)
	gate.await(t)
	a, ok := intakefixture.ArchiveRecord(t, s.store, member)
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
	gate.release()
	changes, closeChanges := serviceChanges(t, s)
	defer closeChanges()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		a, _ = intakefixture.ArchiveRecord(t, s.store, member)
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
