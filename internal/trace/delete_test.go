package trace

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestWorkstreamTraceDeletionExcludesScopedOpen(t *testing.T) {
	t.Parallel()
	r, root, p := create(t)
	live := mustScoped(t, r, streamID)
	if got := mustScoped(t, r, streamID); got != live {
		t.Fatal("scoped did not reuse the live handle before eviction")
	}
	evicted, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	deleted := make(chan error, 1)
	go func() {
		moved, err := r.markWorkstreamTraceForDeletion(streamID, func() {
			// At this boundary the old handle still owns its flock. A new
			// scoped open must wait for the rename, not try to reacquire it.
			if r.workstreamsMu.TryLock() {
				r.workstreamsMu.Unlock()
				t.Error("eviction released scoped-open exclusion before closing the live trace")
			}
			close(evicted)
			<-release
		})
		if err == nil && !moved {
			t.Error("live trace was not moved")
		}
		deleted <- err
	}()
	select {
	case <-evicted:
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not reach eviction")
	}
	opened := make(chan error, 1)
	go func() {
		_, err := r.scoped(streamID)
		opened <- err
	}()
	release <- struct{}{}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not finish")
	}
	select {
	case err := <-opened:
		if errors.Is(err, ErrLocked) || !os.IsNotExist(err) {
			t.Fatalf("scoped after eviction: %v, want not-found", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scoped did not finish after deletion")
	}
	if moved, err := r.MarkWorkstreamTraceForDeletion(streamID); err != nil || moved {
		t.Fatalf("repeated mark: moved=%v, err=%v", moved, err)
	}
	if pending, err := WorkstreamTraceDeletionPending(root, p.ID, streamID); err != nil || !pending {
		t.Fatalf("deletion pending: %v, %v", pending, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if moved, err := reopened.MarkWorkstreamTraceForDeletion(streamID); err != nil || moved {
		t.Fatalf("mark after restart: moved=%v, err=%v", moved, err)
	}
	if err := FinishWorkstreamTraceDeletion(root, p.ID, streamID); err != nil {
		t.Fatal(err)
	}
	if gone, err := WorkstreamTraceGone(root, p.ID, streamID); err != nil || !gone {
		t.Fatalf("trace gone: %v, %v", gone, err)
	}
}
