package trace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestMemoryWatchCommitsCursorAndTerminalChiefNoticesTogether(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	h := header("transition", "delivered")
	if _, err := r.SetFeatureState(ctx, h, "delivered", "Published"); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	events := []MemoryEvent{{Event: "event-1", Document: "document-1", Kind: "message", Source: "discord", Time: at}}
	if err := r.RecordMemoryWatch(ctx, key, "", "cursor-1", events, []config.WorkstreamID{streamID}, at); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if cursor, err := r.MemoryCursor(key); err != nil || cursor != "cursor-1" {
		t.Fatalf("cursor %q %v", cursor, err)
	}
	if err := r.RecordMemoryWatch(ctx, key, "", "cursor-1", events, []config.WorkstreamID{streamID}, at); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordMemoryWatch(ctx, key, "cursor-1", "cursor-2", events, []config.WorkstreamID{streamID}, at); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordMemoryWatch(ctx, key, "cursor-1", "cursor-3", events, []config.WorkstreamID{streamID}, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale cursor %v", err)
	}
	outbox, err := r.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, event := range outbox {
		if strings.HasPrefix(event.TransitionID, "watch-notice-") {
			notices++
			if !strings.Contains(event.Event.Body, "external evidence") || !strings.Contains(event.Event.Body, "event-1") {
				t.Fatal(event.Event.Body)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("%d notices", notices)
	}
	feature, err := r.Workflow(streamID, FeatureSubject)
	if err != nil || feature.Value != "delivered" {
		t.Fatalf("terminal feature %+v %v", feature, err)
	}
	if err := r.RecordMemoryWatch(ctx, key, "cursor-2", "cursor-3", []MemoryEvent{{Event: "missing-document"}}, []config.WorkstreamID{streamID}, at); err == nil {
		t.Fatal("invalid response accepted")
	}
	if cursor, _ := r.MemoryCursor(key); cursor != "cursor-2" {
		t.Fatal("invalid response advanced cursor")
	}
}
