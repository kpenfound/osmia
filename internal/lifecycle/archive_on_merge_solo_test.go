package lifecycle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAutoArchiveSoloWorkstreamAndItsConversationUntilCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := journey.ProjectFixture(t)
	host := &branchLookups{FakePulls: &fixturekit.FakePulls{}}
	opts.PullRequests = host
	gate := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(gate.Release)
	opts.Boundary = gate.Checkpoint
	s, c := journey.StartT(t, opts)
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	repo := s.Sole().Repository
	member := config.WorkstreamID("w_00000000000000000000000000000992")
	fixturekit.Must(t, repo.CreateWorkstream(ctx, member, s.Now(), intake.OwnerActor))
	_, err = trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, s.Now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/osmia", Fork: "owner/fork", Base: "main", Branch: intake.FeatureBranch(member), Commit: "feature-tip", PullRequest: 7, URL: "https://github.com/owner/osmia/pull/7"}
	journey.ResolutionDocument(t, repo, member, delivery.PublicationDocument, delivery.PublicationPath, publication)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: s.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "pull request published")
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.RefreshRuntime())
	host.Mu.Lock()
	host.Prs = append(host.Prs, mergedPullRequest(publication, "solo-merge"))
	host.Mu.Unlock()

	changes, closeChanges := journey.ServiceChanges(t, s)
	defer closeChanges()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	var a runtime.Archive
	for {
		var ok bool
		if a, ok = intakefixture.ArchiveRecord(t, s.Store, member); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the chief of staff did not archive the merged workstream")
		}
		changes()
	}
	gate.Await(t)
	if a.Actor != runtime.ArchiveActorManager || a.State != intake.DeliveredState || a.DeliveryLink != publication.URL || a.ResolutionEvidence != "merged" || !a.Resolved ||
		a.Publication != (runtime.ArchivePublication{Upstream: "owner/osmia", Fork: "owner/fork", Branch: intake.FeatureBranch(member), Commit: "feature-tip", PullRequest: 7}) ||
		a.MergeCommit != "solo-merge" || a.MergeReference != "owner/osmia#7" {
		t.Fatalf("automatic archive=%+v", a)
	}
	if host.lookups(intake.FeatureBranch(member)) != 1 {
		t.Fatalf("pull request lookups for the workstream: %d", host.lookups(intake.FeatureBranch(member)))
	}

	view, err := c.Status(ctx, member)
	fixturekit.Must(t, err)
	if !view.Archived || view.ArchivedBy != api.ArchivedByManager {
		t.Fatalf("status=%+v", view)
	}
	conversation, err := c.Conversation(ctx, member)
	fixturekit.Must(t, err)
	entries := archiveEntries(conversation.Entries, member)
	if len(entries) != 1 || entries[0].Kind != "action" || !strings.Contains(entries[0].Text, publication.URL) || !strings.Contains(entries[0].Text, "because its pull request merged") {
		t.Fatalf("conversation before cleanup: %+v", conversation.Entries)
	}

	gate.Release()
	for {
		if a, _ = intakefixture.ArchiveRecord(t, s.Store, member); !a.CleanedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not complete")
		}
		changes()
	}
	if _, err := c.Conversation(ctx, member); err == nil {
		t.Fatal("the conversation survived archive cleanup")
	}
	if a.Actor != runtime.ArchiveActorManager || archiveCount(s.Store, member) != 1 {
		t.Fatalf("archive after cleanup=%+v", a)
	}
	if host.lookups(intake.FeatureBranch(member)) != 1 {
		t.Fatalf("the archive looked the pull request up again: %d", host.lookups(intake.FeatureBranch(member)))
	}
}
