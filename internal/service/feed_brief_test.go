package service

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func briefFeedEntries(entries []FeedEntry) []FeedEntry {
	var out []FeedEntry
	for _, e := range entries {
		if e.Kind == "brief" {
			out = append(out, e)
		}
	}
	return out
}

// The brief recorded for a final review is its own entry of the member's feed
// with its text, items and screenshots, and a replacement is still one entry.
func TestFeedShowsTheRecordedBriefAsItsOwnEntry(t *testing.T) {
	t.Parallel()
	f, stream, repository, report, tool := briefFixture(t)
	p, api := f.s.deliveryPresentation(context.Background(), string(stream))
	if api != nil {
		t.Fatal(api)
	}
	feed, api := f.s.feedList(string(stream))
	if api != nil || len(briefFeedEntries(feed.Entries)) != 0 {
		t.Fatalf("a feed without a brief has brief entries: %v", api)
	}
	shot := previewCapture(t, repository, stream, report.Commit, "home")
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": p.ReviewRevision, "text": "First.", "before_you_ship": []string{"Restart"}, "screenshots": []string{shot}}); !result.Recorded {
		t.Fatalf("first: %+v", result)
	}
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": p.ReviewRevision, "text": "Second.", "before_you_ship": []string{"Set A", "Set B"}, "screenshots": []string{shot}}); !result.Recorded {
		t.Fatalf("second: %+v", result)
	}
	feed, api = f.s.feedList(string(stream))
	if api != nil {
		t.Fatal(api)
	}
	briefs := briefFeedEntries(feed.Entries)
	if len(briefs) != 1 {
		t.Fatalf("%d brief entries, want the latest brief of the review only: %+v", len(briefs), briefs)
	}
	b := briefs[0].Brief
	if b == nil || b.Review != 1 || b.ReviewRevision != p.ReviewRevision || b.Commit != report.Commit || b.Text != "Second." || !slices.Equal(b.BeforeYouShip, []string{"Set A", "Set B"}) {
		t.Fatalf("brief: %+v", b)
	}
	if len(b.Screenshots) != 1 || b.Screenshots[0].ID != shot || b.Screenshots[0].URL != imageURL(stream, shot) || b.Screenshots[0].ContentType != "image/png" {
		t.Fatalf("screenshots: %+v", b.Screenshots)
	}
}

// Each member's briefs show in the shared chief's workgroup feed labelled with
// that member, and never in another member's feed.
func TestWorkgroupFeedLabelsEachMembersBrief(t *testing.T) {
	t.Parallel()
	s, _, group := workgroupFeedFixture(t, false, 2)
	_, _, repo, api := s.conversationTrace(string(group.Members[0]))
	if api != nil {
		t.Fatal(api)
	}
	fixturekit.FeedDocument(t, repo, fixturekit.Quiet, delivery.BriefDocument(1), delivery.BriefPath(1), 1, demoStart, delivery.Brief{Review: 1, ReviewRevision: 2, Commit: "abc", Text: "Quiet's brief", BeforeYouShip: []string{"Run migration"}})
	var feed WorkgroupFeedResponse
	if code := feedHTTP(t, s, http.MethodGet, Prefix+"/workgroups/"+string(group.ID)+"/feed", "", &feed); code != 200 {
		t.Fatalf("feed status %d", code)
	}
	briefs := briefFeedEntries(feed.Entries)
	if len(briefs) != 1 || briefs[0].Workstream != fixturekit.Quiet || briefs[0].Repo != repo.Project() || briefs[0].Brief.Text != "Quiet's brief" || !slices.Equal(briefs[0].Brief.BeforeYouShip, []string{"Run migration"}) {
		t.Fatalf("workgroup feed briefs: %+v", briefs)
	}
	other, api := s.feedList(string(group.Members[0]))
	if api != nil || len(briefFeedEntries(other.Entries)) != 0 {
		t.Fatalf("another member's feed shows the brief: %v", api)
	}
}
