package browsertest

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/service"
)

func TestBrowserCapacityNamesWaitingPreview(t *testing.T) {
	opts, _ := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, _ := start(t, opts)
	access := service.InspectForTest(s)
	access.ResizeCheckSlots(1)
	release, err := access.AcquireCheckSlot(t.Context(), service.CheckSlotRun{Project: project, Workstream: stream, Unit: "holder"})
	must(t, err)
	defer release()
	p := openOutcomePage(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		release, err := access.AcquireCheckSlot(ctx, service.CheckSlotRun{Project: project, Workstream: stream, Preview: true})
		if release != nil {
			release()
		}
		done <- err
	}()
	p.AwaitText("#capacity [data-role=checks] [data-field=waiting]", "preview: every slot is taken")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("preview did not cancel")
	}
	p.AwaitText("#capacity [data-role=checks]", "Nothing waits.")
}
