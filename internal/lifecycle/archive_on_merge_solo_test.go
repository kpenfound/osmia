package lifecycle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// A delivered workstream with a pull request of its own is archived by the
// running service's background passes once the host reports the pull request
// merged: no owner action, the chief of staff on the record, and its
// conversation reading the entry until archive cleanup removes it.
func TestAutoArchiveSoloWorkstreamAndItsConversationUntilCleanup(t *testing.T) {
	ctx := context.Background()
	opts, clone := projectFixture(t)
	host := &branchLookups{fakePulls: &fakePulls{}}
	opts.PullRequests = host
	gate := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(gate.release)
	opts.boundary = gate.checkpoint
	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	repo := s.sole().repository
	member := config.WorkstreamID("w_00000000000000000000000000000992")
	must(t, repo.CreateWorkstream(ctx, member, s.now(), ownerActor))
	_, err = trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, s.now(), ownerActor, "fixture")
	must(t, err)
	publication := DeliveryPublication{Status: publicationOpened, Upstream: "owner/osmia", Fork: "owner/fork", Base: "main", Branch: featureBranch(member), Commit: "feature-tip", PullRequest: 7, URL: "https://github.com/owner/osmia/pull/7"}
	resolutionDocument(t, repo, member, publicationDocument, publicationPath, publication)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: added.Project.ID, Workstream: member, At: s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "pull request published")
	must(t, err)
	must(t, s.refreshRuntime())
	host.Mu.Lock()
	host.Prs = append(host.Prs, mergedPullRequest(publication, "solo-merge"))
	host.Mu.Unlock()

	changes, closeChanges := serviceChanges(t, s)
	defer closeChanges()
	deadline := time.Now().Add(demoTimeout)
	var a runtime.Archive
	for {
		var ok bool
		if a, ok = archiveRecord(t, s.store, member); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the chief of staff did not archive the merged workstream")
		}
		changes()
	}
	gate.await(t)
	if a.Actor != runtime.ArchiveActorChief || a.State != DeliveredState || a.DeliveryLink != publication.URL || a.ResolutionEvidence != "merged" || !a.Resolved ||
		a.Publication != (runtime.ArchivePublication{Upstream: "owner/osmia", Fork: "owner/fork", Branch: featureBranch(member), Commit: "feature-tip", PullRequest: 7}) ||
		a.MergeCommit != "solo-merge" || a.MergeReference != "owner/osmia#7" {
		t.Fatalf("automatic archive=%+v", a)
	}
	if host.lookups(featureBranch(member)) != 1 {
		t.Fatalf("pull request lookups for the workstream: %d", host.lookups(featureBranch(member)))
	}

	view, err := c.Status(ctx, member)
	must(t, err)
	if !view.Archived || view.ArchivedBy != api.ArchivedByChief {
		t.Fatalf("status=%+v", view)
	}
	conversation, err := c.Conversation(ctx, member)
	must(t, err)
	entries := archiveEntries(conversation.Entries, member)
	if len(entries) != 1 || entries[0].Kind != "action" || !strings.Contains(entries[0].Text, publication.URL) || !strings.Contains(entries[0].Text, "because its pull request merged") {
		t.Fatalf("conversation before cleanup: %+v", conversation.Entries)
	}

	gate.release()
	for {
		if a, _ = archiveRecord(t, s.store, member); !a.CleanedAt.IsZero() {
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
	if a.Actor != runtime.ArchiveActorChief || archiveCount(s.store, member) != 1 {
		t.Fatalf("archive after cleanup=%+v", a)
	}
	if host.lookups(featureBranch(member)) != 1 {
		t.Fatalf("the archive looked the pull request up again: %d", host.lookups(featureBranch(member)))
	}
}
