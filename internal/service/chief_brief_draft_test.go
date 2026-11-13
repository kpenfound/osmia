package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A brief with Before you ship items recorded or replaced after the owner read
// the presentation changes the draft hash, so an approval carrying the earlier
// hash is refused with the existing conflict; the brief's other fields reach
// neither the drafted description nor the delivery commit messages.
func TestDeliveryBriefItemsChangeTheDraftHashAndRefuseAnEarlierApproval(t *testing.T) {
	t.Parallel()
	f, stream, _, report, tool := briefFixture(t)
	ctx := context.Background()
	present := func() DeliveryPresentation {
		t.Helper()
		p, api := f.s.deliveryPresentation(ctx, string(stream))
		if api != nil {
			t.Fatal(api)
		}
		return p
	}
	refused := func(p DeliveryPresentation, hash string) {
		t.Helper()
		_, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: 1, ReviewRevision: p.ReviewRevision, Commit: report.Commit, DraftHash: hash})
		if api == nil || api.Code != Conflict || !strings.Contains(api.Message, "the final report or draft description changed") {
			t.Fatalf("approval with the earlier hash: %v", api)
		}
	}
	read := present()
	const mainText = "Resumable uploads now continue after a dropped connection."
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": read.ReviewRevision, "text": mainText}); !result.Recorded {
		t.Fatalf("brief without items: %+v", result)
	}
	if p := present(); p.DraftHash != read.DraftHash || p.Draft != read.Draft {
		t.Fatal("a brief without items changed the draft")
	}
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": read.ReviewRevision, "text": mainText, "before_you_ship": []string{"Set UPLOAD_KEY"}}); !result.Recorded {
		t.Fatalf("brief with items: %+v", result)
	}
	first := present()
	if first.DraftHash == read.DraftHash || !strings.Contains(first.Draft, "\nBefore you ship:\n- Set UPLOAD_KEY\n") {
		t.Fatalf("the items did not enter the draft:\n%s", first.Draft)
	}
	refused(first, read.DraftHash)
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": read.ReviewRevision, "text": mainText, "before_you_ship": []string{"Set UPLOAD_KEY", "Restart the worker"}}); !result.Recorded {
		t.Fatalf("replacement: %+v", result)
	}
	second := present()
	if second.DraftHash == first.DraftHash || !strings.Contains(second.Draft, "- Set UPLOAD_KEY\n- Restart the worker\n") {
		t.Fatalf("the replacement did not change the draft:\n%s", second.Draft)
	}
	refused(second, first.DraftHash)
	messages, err := json.Marshal(second.Messages)
	must(t, err)
	for _, text := range []string{mainText, "Set UPLOAD_KEY"} {
		if strings.Contains(second.Title, text) || strings.Contains(string(messages), text) || strings.Contains(strings.ReplaceAll(second.Draft, "- Set UPLOAD_KEY", ""), text) {
			t.Fatalf("%q reached the title, body or commit messages", text)
		}
	}
	approved, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: 1, ReviewRevision: second.ReviewRevision, Commit: report.Commit, DraftHash: second.DraftHash})
	if api != nil || approved.Description != second.Draft {
		t.Fatalf("approval of the current presentation: %v", api)
	}
}
