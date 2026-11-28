package lifecycle_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func readWorkgroupFeed(t *testing.T, f *journey.ProductionDesignFixture, group config.WorkgroupID) api.WorkgroupFeedResponse {
	t.Helper()
	var feed api.WorkgroupFeedResponse
	fixturekit.Must(t, f.C.Do(context.Background(), "GET", api.Prefix+"/workgroups/"+string(group)+"/feed", nil, &feed))
	return feed
}

func feedArchiveEntries(feed api.WorkgroupFeedResponse, member config.WorkstreamID) []api.FeedEntry {
	var out []api.FeedEntry
	for _, e := range feed.Entries {
		if e.Kind == "action" && strings.Contains(e.Text, "Archived workstream "+string(member)) {
			out = append(out, e)
		}
	}
	return out
}

func awaitEveryMemberCleaned(t *testing.T, f *journey.ProductionDesignFixture) {
	t.Helper()
	journey.LifecycleAwait(t, f, "every member's cleanup", func() bool {
		for _, m := range resolutionMembers(f) {
			a, ok := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
			if !ok || a.CleanedAt.IsZero() {
				return false
			}
		}
		return true
	})
}

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

func TestSharedFeedStaysReadableAfterEveryMemberIsAutoArchived(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	group := resolutionGroupID(t, g.ProductionDesignFixture)
	before := retainedEntries(readWorkgroupFeed(t, g.ProductionDesignFixture, group))
	if len(before) == 0 {
		t.Fatal("the shared feed is empty before the members are archived")
	}
	g.host.Mu.Lock()
	g.host.Prs = []pulls.PullRequest{mergedPullRequest(g.pubs[0], "first-merge"), mergedPullRequest(g.pubs[1], "second-merge")}
	g.host.Mu.Unlock()
	journey.LifecycleAwait(t, g.f(), "the chief of staff archives every member", func() bool {
		for _, m := range resolutionMembers(g.f()) {
			if a, ok := intakefixture.ArchiveRecord(t, g.S.Store, m.stream); !ok || a.Actor != runtime.ArchiveActorManager {
				return false
			}
		}
		return true
	})
	g.gate.Release()
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
			if !m.Archived || m.ArchivedBy != api.ArchivedByManager {
				t.Fatalf("%s: member summary=%+v", phase, m)
			}
		}
		conversation, err := g.C.Conversation(ctx, g.Origin)
		fixturekit.Must(t, err)
		for _, m := range resolutionMembers(g.f()) {
			if entries := archiveEntries(conversation.Entries, m.stream); len(entries) != 1 {
				t.Fatalf("%s: conversation entries of %s: %+v", phase, m.stream, conversation.Entries)
			}
		}
	}
	check("after cleanup")

	count := len(readWorkgroupFeed(t, g.f(), group).Entries)
	var sent api.ConversationEntry
	if err := g.C.Do(ctx, "POST", api.Prefix+"/workgroups/"+string(group)+"/conversation", api.SendRequest{Text: "Anyone still here?"}, &sent); err == nil {
		t.Fatalf("sending to a fully archived workgroup was accepted: %+v", sent)
	}
	if after := readWorkgroupFeed(t, g.f(), group); len(after.Entries) != count || slices.ContainsFunc(after.Entries, func(e api.FeedEntry) bool { return e.Text == "Anyone still here?" }) {
		t.Fatalf("a refused send changed the shared thread: %d -> %d entries", count, len(after.Entries))
	}

	g.f().Stop(t)
	g.f().Start(t)
	check("after restart")
}

func TestSharedFeedStaysReadableAfterTheOwnerArchivesEveryMember(t *testing.T) {
	t.Parallel()
	g := newAutoArchiveGroup(t)
	group := resolutionGroupID(t, g.ProductionDesignFixture)
	before := retainedEntries(readWorkgroupFeed(t, g.ProductionDesignFixture, group))
	if len(before) == 0 {
		t.Fatal("the shared feed is empty before the members are archived")
	}
	for _, m := range resolutionMembers(g.f()) {
		repo, err := g.S.Repository(m.project)
		fixturekit.Must(t, err)
		journey.RecordResolutionMerge(t, repo, m.stream)
	}
	down, err := g.S.Repository(g.Project2)
	fixturekit.Must(t, err)
	journey.LifecycleAwait(t, g.f(), "the downstream edge is released", func() bool {
		wait, err := lifecycle.EdgeWaitDisplay(down, g.Stream2)
		fixturekit.Must(t, err)
		return wait == ""
	})
	for _, m := range resolutionMembers(g.f()) {
		resp, err := g.C.Archive(context.Background(), m.stream)
		fixturekit.Must(t, err)
		if resp.ArchivedBy != api.ArchivedByOwner {
			t.Fatalf("owner archive reports %+v", resp)
		}
	}
	g.gate.Release()
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
