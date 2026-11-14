package lifecycle_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
)

func readWorkgroupFeed(t *testing.T, f *productionDesignFixture, group config.WorkgroupID) api.WorkgroupFeedResponse {
	t.Helper()
	var feed api.WorkgroupFeedResponse
	must(t, f.c.Do(context.Background(), "GET", api.Prefix+"/workgroups/"+string(group)+"/feed", nil, &feed))
	return feed
}

// feedArchiveEntries returns the shared feed's entries announcing the archive
// of member.
func feedArchiveEntries(feed api.WorkgroupFeedResponse, member config.WorkstreamID) []api.FeedEntry {
	var out []api.FeedEntry
	for _, e := range feed.Entries {
		if e.Kind == "action" && strings.Contains(e.Text, "Archived workstream "+string(member)) {
			out = append(out, e)
		}
	}
	return out
}

func awaitEveryMemberCleaned(t *testing.T, f *productionDesignFixture) {
	t.Helper()
	lifecycleAwait(t, f, "every member's cleanup", func() bool {
		for _, m := range resolutionMembers(f) {
			a, ok := archiveRecord(t, f.s.store, m.stream)
			if !ok || a.CleanedAt.IsZero() {
				return false
			}
		}
		return true
	})
}

// retainedEntries lists the entries of before that are not archive
// announcements.
func retainedEntries(before api.WorkgroupFeedResponse) []api.FeedEntry {
	return slices.DeleteFunc(slices.Clone(before.Entries), func(e api.FeedEntry) bool {
		return e.Kind == "action" && strings.Contains(e.Text, "Archived workstream ")
	})
}

func requireRetained(t *testing.T, feed api.WorkgroupFeedResponse, before []api.FeedEntry) {
	t.Helper()
	for _, earlier := range before {
		if !slices.ContainsFunc(feed.Entries, func(e api.FeedEntry) bool {
			return e.Turn == earlier.Turn && e.Kind == earlier.Kind && e.Text == earlier.Text && e.Workstream == earlier.Workstream
		}) {
			t.Fatalf("the shared feed lost %+v: %+v", earlier, feed.Entries)
		}
	}
}

// Once the chief of staff has archived every member and cleanup has run, the
// shared conversation is still read, with each automatic-archive entry once. It
// stays so across a restart, and sending to the group is refused without
// adding an entry.
func TestSharedFeedStaysReadableAfterEveryMemberIsAutoArchived(t *testing.T) {
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	group := resolutionGroupID(t, g.productionDesignFixture)
	before := retainedEntries(readWorkgroupFeed(t, g.productionDesignFixture, group))
	if len(before) == 0 {
		t.Fatal("the shared feed is empty before the members are archived")
	}
	g.host.Mu.Lock()
	g.host.Prs = []pulls.PullRequest{mergedPullRequest(g.pubs[0], "first-merge"), mergedPullRequest(g.pubs[1], "second-merge")}
	g.host.Mu.Unlock()
	lifecycleAwait(t, g.f(), "the chief of staff archives every member", func() bool {
		for _, m := range resolutionMembers(g.f()) {
			if a, ok := archiveRecord(t, g.s.store, m.stream); !ok || a.Actor != runtime.ArchiveActorChief {
				return false
			}
		}
		return true
	})
	g.gate.release()
	awaitEveryMemberCleaned(t, g.f())

	check := func(phase string) {
		t.Helper()
		feed := readWorkgroupFeed(t, g.f(), group)
		requireRetained(t, feed, before)
		for _, m := range resolutionMembers(g.f()) {
			entries := feedArchiveEntries(feed, m.stream)
			if len(entries) != 1 || entries[0].Workstream != m.stream || entries[0].Repo != m.project {
				t.Fatalf("%s: archive entries of %s: %+v", phase, m.stream, entries)
			}
		}
		if len(feed.Members) != 2 {
			t.Fatalf("%s: members=%+v", phase, feed.Members)
		}
		for _, m := range feed.Members {
			if !m.Archived || m.ArchivedBy != api.ArchivedByChief {
				t.Fatalf("%s: member summary=%+v", phase, m)
			}
		}
		// The conversation of the origin reads the same thread.
		conversation, err := g.c.Conversation(ctx, g.origin)
		must(t, err)
		for _, m := range resolutionMembers(g.f()) {
			if entries := archiveEntries(conversation.Entries, m.stream); len(entries) != 1 {
				t.Fatalf("%s: conversation entries of %s: %+v", phase, m.stream, conversation.Entries)
			}
		}
	}
	check("after cleanup")

	count := len(readWorkgroupFeed(t, g.f(), group).Entries)
	var sent api.ConversationEntry
	if err := g.c.Do(ctx, "POST", api.Prefix+"/workgroups/"+string(group)+"/conversation", api.SendRequest{Text: "Anyone still here?"}, &sent); err == nil {
		t.Fatalf("sending to a fully archived workgroup was accepted: %+v", sent)
	}
	if after := readWorkgroupFeed(t, g.f(), group); len(after.Entries) != count || slices.ContainsFunc(after.Entries, func(e api.FeedEntry) bool { return e.Text == "Anyone still here?" }) {
		t.Fatalf("a refused send changed the shared thread: %d -> %d entries", count, len(after.Entries))
	}

	g.f().stop(t)
	g.f().start(t)
	check("after restart")
}

// An owner archive of the last member leaves the retained shared conversation
// readable and adds no automatic-archive entry.
func TestSharedFeedStaysReadableAfterTheOwnerArchivesEveryMember(t *testing.T) {
	g := newAutoArchiveGroup(t)
	group := resolutionGroupID(t, g.productionDesignFixture)
	before := retainedEntries(readWorkgroupFeed(t, g.productionDesignFixture, group))
	if len(before) == 0 {
		t.Fatal("the shared feed is empty before the members are archived")
	}
	// Each member carries an owner-recorded merge, so every archive pins merge
	// evidence and its cleanup can complete.
	for _, m := range resolutionMembers(g.f()) {
		repo, err := g.s.repository(m.project)
		must(t, err)
		recordResolutionMerge(t, repo, m.stream)
	}
	// The downstream member's dependency edge is released before it is
	// archived: an archived member's edges are no longer refreshed, and its
	// cleanup waits for the edge to be released. The edge is released once it
	// observes its upstream's recorded merge, or once the workgroup is recorded
	// resolved, which leaves a merged member with no edges to wait on. The pull
	// request host reports no merge, so the chief of staff archives nothing.
	down, err := g.s.repository(g.project2)
	must(t, err)
	lifecycleAwait(t, g.f(), "the downstream edge is released", func() bool {
		wait, err := lifecycle.EdgeWaitDisplay(down, g.stream2)
		must(t, err)
		return wait == ""
	})
	for _, m := range resolutionMembers(g.f()) {
		resp, err := g.c.Archive(context.Background(), m.stream)
		must(t, err)
		if resp.ArchivedBy != api.ArchivedByOwner {
			t.Fatalf("owner archive reports %+v", resp)
		}
	}
	g.gate.release()
	awaitEveryMemberCleaned(t, g.f())
	feed := readWorkgroupFeed(t, g.f(), group)
	requireRetained(t, feed, before)
	for _, m := range resolutionMembers(g.f()) {
		if entries := feedArchiveEntries(feed, m.stream); len(entries) != 0 {
			t.Fatalf("an owner archive was announced: %+v", entries)
		}
	}
	for _, m := range feed.Members {
		if !m.Archived || m.ArchivedBy != api.ArchivedByOwner {
			t.Fatalf("member summary=%+v", m)
		}
	}
}
