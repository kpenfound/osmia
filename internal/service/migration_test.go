package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// TestTraceMigrationFailureDoesNotBlockOtherProjects breaks one project's
// trace so that migrating it from the shared layout can never complete,
// before the service starts, and confirms the service still starts, the
// broken project is simply not scheduled, and the other project's
// workstream keeps running normally.
func TestTraceMigrationFailureDoesNotBlockOtherProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)

	// Give the first project a shared-layout workstream directory with no
	// manifest, so its migration fails every time it is attempted.
	directory, err := f.cfg.Root.ProjectTrace(project)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(directory, "workstreams", "w_deadbeefdeadbeefdeadbeefdeadbeef"), 0700))

	otherEntered, otherRelease := f.block(otherProject, "first")
	s, err := Start(ctx, f.opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })

	// The broken project never opens a trace and is never scheduled; the
	// other project runs as usual.
	if _, active := s.runtimeOf(project); active != nil {
		t.Fatalf("a project whose migration cannot complete was scheduled: %+v", active)
	}
	if _, active := s.runtimeOf(otherProject); active == nil {
		t.Fatal("the other project was not scheduled")
	}
	await(t, "the other project's turn", otherEntered)
	close(otherRelease)
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		repo := f.repository(otherProject)
		if repo == nil {
			return nil
		}
		ops, err := repo.Operations(f.streams[otherProject])
		must(t, err)
		return ops
	})

	// The shared-layout fixture is left exactly as it was: nothing scheduled
	// it, so nothing reclaimed it either.
	if _, err := os.Lstat(filepath.Join(directory, "workstreams")); err != nil {
		t.Fatalf("the unmigrated project's shared trace was altered: %v", err)
	}
}
