package service

import (
	"context"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestCheckSlotsCancellationAndAdmissionRace(t *testing.T) {
	t.Parallel()
	for range 100 {
		p := &checkSlotPool{limit: 1}
		holder, err := p.acquire(context.Background(), CheckSlotRun{Unit: "holder"})
		must(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		next := queuedSlot(ctx, p, CheckSlotRun{Unit: "cancelled"})
		// Cancellation may race with the return of capacity, including an
		// already-granted slot whose caller has not observed ready yet.
		var wg sync.WaitGroup
		wg.Go(cancel)
		wg.Go(holder)
		wg.Wait()
		if release := awaitCheck(t, next); release != nil {
			release()
		}
		if st := p.status(); st.Used != 0 || len(st.Waiting) != 0 || len(st.Active) != 0 {
			t.Fatalf("leaked capacity: %+v", st)
		}
	}
}

func queuedSlot(ctx context.Context, p *checkSlotPool, run CheckSlotRun) <-chan func() {
	return fixturekit.QueueSlot(ctx, func(ctx context.Context) (func(), error) { return p.acquire(ctx, run) })
}
