package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

// The delivery command prints the brief text, each "Before you ship" item and
// a link per screenshot, or says why there is no brief.
func TestDeliveryPrintsTheBrief(t *testing.T) {
	for _, tc := range []struct {
		name    string
		brief   *wire.DeliveryBrief
		want    []string
		missing []string
	}{
		{"recorded", &wire.DeliveryBrief{State: wire.BriefRecorded, Review: 2, ReviewRevision: 3, Commit: "abc123", Text: "Owners see the new page.",
			BeforeYouShip: []string{"Set OSMIA_TOKEN", "Restart the service"}, Screenshots: []string{"img1", "img2"}},
			[]string{"Owners see the new page.", "Before you ship:", "  - Set OSMIA_TOKEN", "  - Restart the service",
				"Screenshot: /v1/images/" + stream + "/img1", "Screenshot: /v1/images/" + stream + "/img2"},
			[]string{wire.NoBriefRecorded}},
		{"recorded without items", &wire.DeliveryBrief{State: wire.BriefRecorded, Review: 2, ReviewRevision: 3, Commit: "abc123", Text: "Text only."},
			[]string{"Text only."}, []string{"Before you ship", "Screenshot:", wire.NoBriefRecorded}},
		{"none recorded", &wire.DeliveryBrief{State: wire.BriefNoneRecorded, Review: 2, ReviewRevision: 3, Commit: "abc123"},
			[]string{wire.NoBriefRecorded}, []string{"Before you ship"}},
		{"waiting", &wire.DeliveryBrief{State: wire.BriefWaiting, Review: 2, ReviewRevision: 3, Commit: "abc123"},
			[]string{"still writing"}, []string{wire.NoBriefRecorded}},
		{"no field", nil, nil, []string{wire.NoBriefRecorded, "Delivery brief"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			presentation := wire.DeliveryPresentation{Workstream: config.WorkstreamID(stream), ReviewRevision: 3, Draft: "The description.\n", Brief: tc.brief,
				Report: wire.FinalReport{Review: 2, Commit: "abc123", Summary: "Everything shown."}}
			root, socket := ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path != "GET /v1/delivery/"+stream {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode(presentation)
			}))
			out := successful(t, root, "delivery", stream, "--socket", socket)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.missing {
				if strings.Contains(out, absent) {
					t.Fatalf("output has %q:\n%s", absent, out)
				}
			}
			if !strings.Contains(out, "Final review 2 of "+stream) {
				t.Fatalf("the report is missing:\n%s", out)
			}
		})
	}
}

// The feed command prints a brief entry with its items and screenshot links.
func TestFeedPrintsTheBriefEntry(t *testing.T) {
	var out strings.Builder
	showFeed(&out, []wire.FeedEntry{{Kind: "brief", Brief: &wire.FeedBrief{Review: 2, Text: "Owners see the new page.", BeforeYouShip: []string{"Set OSMIA_TOKEN"},
		Screenshots: []wire.FeedImage{{ID: "img1", URL: "/v1/images/" + stream + "/img1"}}}}})
	for _, want := range []string{" brief\n", "Delivery brief for final review 2:", "Owners see the new page.", "Before you ship: Set OSMIA_TOKEN", "[image: /v1/images/" + stream + "/img1]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("feed output missing %q:\n%s", want, out.String())
		}
	}
}
