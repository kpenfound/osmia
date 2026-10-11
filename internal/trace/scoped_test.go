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

// TestScopedFirstOpenFailureInsideSerializeReturnsAnError exercises scoped's
// failure path with the project's own operationMu already held, the way a
// reconciliation pass's schedule hook holds it through Serialize. Before
// scoped released a half-opened instance through the ordinary Close, which
// takes the shared operationMu, this deadlocked; it must now return the
// open's error within a bounded wait instead.
func TestScopedFirstOpenFailureInsideSerializeReturnsAnError(t *testing.T) {
	r, _, _ := create(t)
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	dir, err := r.root.WorkstreamTrace(r.project, other)
	if err != nil {
		t.Fatal(err)
	}
	// A directory with no ".git" of its own fails scoped's first checkGit,
	// so this is a first-time open, not a cached one.
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- r.Serialize(func() error {
			_, err := r.scoped(other)
			return err
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("scoped on a broken, not-yet-opened workstream succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed first-time open inside Serialize did not return within the bound; it deadlocked")
	}

	// The project instance is still usable: the failed attempt released its
	// own lock without ever taking the shared one a second time.
	if err := r.Serialize(func() error { return nil }); err != nil {
		t.Fatalf("Serialize after the failed open: %v", err)
	}
	if _, err := r.scoped(streamID); err != nil {
		t.Fatalf("scoped on the existing workstream after the failed open: %v", err)
	}
}

// TestScopedRefusesAfterCloseAndLeaksNoFlock races scoped against Close on
// the same project instance and confirms that whichever runs first, no
// workstream is ever opened twice and nothing of Close's is left for a
// racing scoped to miss: the project's trace opens cleanly again in the same
// process, which a leaked flock on a workstream Close never joined would
// refuse.
func TestScopedRefusesAfterCloseAndLeaksNoFlock(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
		t.Fatal(err)
	}
	// CreateWorkstream eagerly opened and cached other's instance; release
	// its flock and forget it, so the race below makes scoped open it for
	// the first time, as it would for a workstream this instance has not
	// touched yet, rather than return the already-cached one.
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

	// Whichever of the two ran first, the project's trace, including the
	// workstream's own flock, is free again: a fresh Open in this same
	// process succeeds.
	_ = scopedErr
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatalf("reopen after scoped raced Close: %v", err)
	}
	defer reopened.Close()
	if streams, err := reopened.Workstreams(); err != nil || !slices.Contains(streams, other) || !slices.Contains(streams, streamID) {
		t.Fatalf("workstreams after reopening = %v, %v", streams, err)
	}

	// Once Close has fully run, scoped refuses rather than opening a new,
	// now-unjoined child.
	if _, err := r.scoped(other); err == nil {
		t.Fatal("scoped opened a child after Close completed")
	}
}
