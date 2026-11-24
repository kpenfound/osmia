package trace

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func TestScopedFirstOpenFailureReturnsAnError(t *testing.T) {
	r, _, _ := create(t)
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	dir, err := r.root.WorkstreamTrace(r.project, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.scoped(other)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("scoped on a broken, not-yet-opened workstream succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed first-time open did not return within the bound; it deadlocked")
	}

	if _, err := r.scoped(streamID); err != nil {
		t.Fatalf("scoped on the existing workstream after the failed open: %v", err)
	}
}

func TestScopedRefusesAfterCloseAndLeaksNoFlock(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
		t.Fatal(err)
	}
	r.workstreamsMu.Lock()
	cached := r.workstreams[other]
	delete(r.workstreams, other)
	r.workstreamsMu.Unlock()
	if cached != nil {
		if err := cached.closeFiles(); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var scopedErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, scopedErr = r.scoped(other)
	}()
	go func() {
		defer wg.Done()
		<-start
		r.Close()
	}()
	close(start)
	wg.Wait()

	_ = scopedErr
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatalf("reopen after scoped raced Close: %v", err)
	}
	defer reopened.Close()
	if streams, err := reopened.Workstreams(); err != nil || !slices.Contains(streams, other) || !slices.Contains(streams, streamID) {
		t.Fatalf("workstreams after reopening = %v, %v", streams, err)
	}

	if _, err := r.scoped(other); err == nil {
		t.Fatal("scoped opened a child after Close completed")
	}
}
