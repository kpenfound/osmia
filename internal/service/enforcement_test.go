package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// Enforce's thread turns take their settings from the configuration the
// service hands them, not from the disk, select views of their own project
// only and record UTC times.
func TestEnforceThreadsUseLoadedConfiguration(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	runnerIntent(t, opts)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	chief := cfg.Roles[trace.ChiefOfStaff]
	chief.Sandbox, chief.Image = "container", "loaded-image"
	cfg.Roles[trace.ChiefOfStaff] = chief
	must(t, os.WriteFile(filepath.Join(opts.Config.Root, "config.toml"), []byte("not toml ["), 0600))

	zone := time.FixedZone("east", 5*60*60)
	opts.Reconciliation.Now = func() time.Time { return time.Date(2026, 9, 17, 12, 0, 0, 0, zone) }
	repository, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repository.Close()
	bound, err := Enforce(opts, Enforcement{}).Threads(repository, cfg)
	must(t, err)
	dispatcher, ok := bound.(thread.Dispatcher)
	if !ok {
		t.Fatalf("threads reconciler %T", bound)
	}
	if now := dispatcher.Runner.Now(); now.Location() != time.UTC {
		t.Fatalf("thread clock %v is not UTC", now)
	}
	turns := dispatcher.Runner.Turns.(*questions.Turns).Turns.(*reportingTurns).Turns.(*isolation.Turns)
	scope := coreadapter.Scope{Project: string(cfg.Project.ID), Workstream: string(stream), Role: trace.ChiefOfStaff}
	selection, err := turns.Select(context.Background(), scope)
	must(t, err)
	if selection.Execution.Mode != "container" || selection.Execution.Image != "loaded-image" {
		t.Fatalf("execution %+v", selection.Execution)
	}
	scope.Project = "other"
	if _, err := turns.Select(context.Background(), scope); err == nil {
		t.Fatal("selected a view of another project")
	}
}
