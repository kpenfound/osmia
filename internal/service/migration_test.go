package service

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/trace/legacytrace"
)

// TestTraceMigrationFailureDoesNotBlockOtherProjects gives one project a Git
// workstream trace whose migration can never complete, before the service
// starts, and confirms the service still starts, the broken project is
// simply not scheduled, and the other project's workstream keeps running
// normally. The migration failure is logged as a structured record through
// the service's own logger.
func TestTraceMigrationFailureDoesNotBlockOtherProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)

	// Give the first project a Git workstream trace whose HEAD holds a file
	// no trace keeps, so its migration fails every time it is attempted.
	const broken = "w_deadbeefdeadbeefdeadbeefdeadbeef"
	directory, err := f.cfg.Root.WorkstreamTrace(project, broken)
	must(t, err)
	must(t, os.MkdirAll(directory, 0700))
	files := legacytrace.Workstream(string(project), broken, demoStart)
	files["workstreams/"+broken+"/unknown.txt"] = []byte("not a trace file\n")
	must(t, legacytrace.Build(directory, files))

	var logged syncBuffer
	f.opts.Logger = slog.New(slog.NewJSONHandler(&logged, nil))

	otherEntered, otherRelease := f.block(otherProject, "first")
	s, err := Start(ctx, f.opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })

	// The migration failure is logged as a structured record naming the
	// project, not a plain-text line from the standard log package.
	decodeStructuredLines(t, logged.String())
	if !strings.Contains(logged.String(), "project trace migration incomplete") || !strings.Contains(logged.String(), string(project)) {
		t.Fatalf("the migration failure was not logged as a structured record: %s", logged.String())
	}

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

	// The Git trace stays authoritative: no database replaced it.
	if _, err := os.Lstat(filepath.Join(directory, ".git")); err != nil {
		t.Fatalf("the unmigrated Git trace was removed: %v", err)
	}
	if ok, err := trace.HasTraceDatabase(directory); err != nil || ok {
		t.Fatalf("the unmigrated trace has a database: %v %v", ok, err)
	}
}
