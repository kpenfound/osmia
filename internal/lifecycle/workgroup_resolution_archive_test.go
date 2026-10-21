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

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func rewriteLegacyResolutionArchive(t *testing.T, f *productionDesignFixture, member config.WorkstreamID) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "runtime.json")
	data, err := os.ReadFile(path)
	must(t, err)
	var state map[string]any
	must(t, json.Unmarshal(data, &state))
	for _, raw := range state["archived"].([]any) {
		a := raw.(map[string]any)
		if a["workstream"] == string(member) {
			a["resolved"] = true
			delete(a, "resolution_evidence")
		}
	}
	data, err = json.Marshal(state)
	must(t, err)
	must(t, os.WriteFile(path, data, 0600))
}
func TestDesignProductionArchiveRequiresPinnedMergeEvidence(t *testing.T) {
	for _, name := range []string{"ready-unmerged", "owner-merged", "own-pr-merged"} {
		merged := name != "ready-unmerged"
		t.Run(name, func(t *testing.T) {
			gate := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
			t.Cleanup(gate.release)
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: gate.checkpoint}, "contract", nil)
			defer f.stop(t)
			deliverResolutionMembers(t, f)
			second, err := f.s.repository(f.project2)
			must(t, err)
			recordResolutionMerge(t, second, f.stream2)
			first, err := f.s.repository(f.project)
			must(t, err)
			if name == "owner-merged" {
				recordResolutionMerge(t, first, f.stream1)
			} else if name == "own-pr-merged" {
				resolutionDocument(t, first, f.stream1, ownPullRequestOutcomeDocument, ownPullRequestOutcomePath, ownPullRequestOutcome{PullRequest: 101, Outcome: ownPullRequestOutcomeMerged, MergeCommit: "merged-upstream"})
			}
			_, err = f.c.Archive(context.Background(), f.stream1)
			must(t, err)
			gate.await(t)
			checkArchive := func(boundary string) {
				t.Helper()
				a, ok := archiveRecord(t, f.s.store, f.stream1)
				if !ok || a.Resolved != merged || archiveResolved(a) != merged {
					t.Fatalf("%s archive=%+v", boundary, a)
				}
				view, err := f.c.Status(context.Background(), f.stream1)
				must(t, err)
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
			// Group-state checks after deletion must not require deleted member history.
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
			lifecycleAwait(t, f, "archive cleanup", func() bool { a, ok := archiveRecord(t, f.s.store, f.stream1); return ok && !a.CleanedAt.IsZero() })
			// Read group authority and the deleted member's pane without recreating history.
			checkDeleted := func() {
				a, ok := archiveRecord(t, f.s.store, f.stream1)
				if !ok {
					t.Fatal("archive disappeared")
				}
				if archiveResolved(a) != merged {
					t.Fatalf("deleted evidence=%+v", a)
				}
				view, err := f.c.Status(context.Background(), f.stream1)
				must(t, err)
				if (view.DisplayState == "delivered (merged)") != merged {
					t.Fatalf("deleted label=%s", view.DisplayState)
				}
				if !merged && !strings.Contains(view.Wait, "no pinned merge or cancel evidence") {
					t.Fatal("deleted member lost its resolution hold diagnostic")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, a.Workgroup)
				must(t, err)
				defer authority.Close()
				ids, rev, err := trace.ResolvedWorkgroups(authority)
				must(t, err)
				active, err := trace.ProjectSpecActive(authority, a.Workgroup)
				must(t, err)
				if merged {
					if !slices.Equal(ids, []config.WorkgroupID{a.Workgroup}) || rev != 1 || active {
						t.Fatal("merged archived member did not resolve once")
					}
				} else if len(ids) != 0 || rev != 0 || !active {
					t.Fatal("retirement or legacy boolean granted resolution")
				}
				gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
				must(t, err)
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
	ctx := context.Background()
	f, member, repo := newCleanupFixture(t, config.WorkspacesGit)
	defer repo.Close()
	for _, unit := range []string{"upload", "audit"} {
		completeMasonTurn(t, f, repo, member, unit, "built "+unit)
	}
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{member}, f.s.now(), ownerActor, "fixture")
	must(t, err)
	if len(groups) == 0 {
		t.Fatal("missing one-member group")
	}
	p := DeliveryPublication{Status: publicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"}
	resolutionDocument(t, repo, member, publicationDocument, publicationPath, p)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ready-delivered", Revision: 1, Project: repo.Project(), Workstream: member, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "ready PR published")
	must(t, err)
	resolver := &workgroupResolver{s: f.s, repository: repo}
	must(t, resolver.Pass(ctx))
	ids, revision, err := trace.ResolvedWorkgroups(repo)
	must(t, err)
	if len(ids) != 0 || revision != 0 {
		t.Fatal("one-member ready PR resolved")
	}
	state := DeliveredState
	view := WorkstreamStatus{Project: repo.Project(), Workstream: member, State: &state}
	must(t, f.s.presentWorkstream(repo, &view))
	if view.DisplayState != "PR open" {
		t.Fatalf("live label=%s", view.DisplayState)
	}
	cleanup := newCleanup(f, repo)
	must(t, cleanup.Pass(ctx))
	if len(cleanup.failures) != 0 {
		t.Fatalf("delivery cleanup=%v", cleanup.failures)
	}
	assertGone(t, filepath.Join(f.s.cfg.Root.String(), branchesDirectory, string(f.project), string(member)))
	for _, unit := range []string{"upload", "audit"} {
		assertUnitWorkspace(t, f, repo, member, unit, false)
	}
	recordResolutionMerge(t, repo, member)
	must(t, resolver.Pass(ctx))
	must(t, resolver.Pass(ctx))
	ids, revision, err = trace.ResolvedWorkgroups(repo)
	must(t, err)
	if len(ids) != 1 || revision != 1 {
		t.Fatalf("one-member merge resolution=%v revision=%d", ids, revision)
	}
}
func TestSingleMemberReadyArchivePinsUnresolved(t *testing.T) {
	ctx := context.Background()
	opts, clone := projectFixture(t)
	gate := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(gate.release)
	opts.boundary = gate.checkpoint
	s, c := start(t, opts)
	defer func() { must(t, s.Close()) }()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	repo := s.sole().repository
	member := config.WorkstreamID("w_00000000000000000000000000000991")
	must(t, repo.CreateWorkstream(ctx, member, s.now(), ownerActor))
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, s.now(), ownerActor, "fixture")
	must(t, err)
	resolutionDocument(t, repo, member, publicationDocument, publicationPath, DeliveryPublication{Status: publicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "ready PR published")
	must(t, err)
	must(t, s.refreshRuntime())
	_, err = c.Archive(ctx, member)
	must(t, err)
	gate.await(t)
	a, ok := archiveRecord(t, s.store, member)
	if !ok || a.Resolved || a.ResolutionEvidence != "unresolved" {
		t.Fatalf("one-member archive=%+v", a)
	}
	view, err := c.Status(ctx, member)
	must(t, err)
	if view.DisplayState != "blocked" || !strings.Contains(view.Wait, "no pinned merge or cancel evidence") {
		t.Fatalf("archived label=%s", view.DisplayState)
	}
	ids, _, err := trace.ResolvedWorkgroups(repo)
	must(t, err)
	if slices.Contains(ids, g.ID) {
		t.Fatal("one-member archive resolved an unmerged PR")
	}
	gate.release()
	changes, closeChanges := serviceChanges(t, s)
	defer closeChanges()
	deadline := time.Now().Add(demoTimeout)
	for {
		a, _ = archiveRecord(t, s.store, member)
		if !a.CleanedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not complete")
		}
		changes()
	}
	ids, _, err = trace.ResolvedWorkgroups(repo)
	must(t, err)
	if slices.Contains(ids, g.ID) {
		t.Fatal("one-member cleanup resolved an unmerged PR")
	}
}
