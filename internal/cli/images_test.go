package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

func TestFeedPrintsImageURLAndRemovedPlaceholder(t *testing.T) {
	var out bytes.Buffer
	showFeed(&out, []api.FeedEntry{
		{Kind: "image", Image: &api.FeedImage{ID: "image-a", URL: "/v1/images/member/image-a"}},
		{Kind: "image", Image: &api.FeedImage{ID: "image-b", Placeholder: "image removed when member was archived"}},
	})
	if !strings.Contains(out.String(), "[image: /v1/images/member/image-a]") || !strings.Contains(out.String(), "image removed when member was archived") {
		t.Fatal(out.String())
	}
}
