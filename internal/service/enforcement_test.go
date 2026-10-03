package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/skills"
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
	chief.Sandbox, chief.Image, chief.Skills = "container", "loaded-image", []string{"https://github.com/acme/skills#skills/triage"}
	cfg.Roles[trace.ChiefOfStaff] = chief
	mason := cfg.Roles[masonRole]
	mason.Sandbox, mason.Dagger, mason.Skills = "sbx", &config.Dagger{Version: "v0.20.5", Engine: "tcp://127.0.0.1:1234"}, []string{"https://github.com/acme/skills#skills/tdd"}
	cfg.Roles[masonRole] = mason
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
	turns := dispatcher.Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
	scope := coreadapter.Scope{Project: string(cfg.Project.ID), Workstream: string(stream), Role: trace.ChiefOfStaff}
	selection, err := turns.Select(context.Background(), scope)
	must(t, err)
	if selection.Execution.Mode != "container" || selection.Execution.Image != "loaded-image" || selection.Execution.Dagger != nil || !slices.Equal(selection.Execution.Skills, chief.Skills) {
		t.Fatalf("execution %+v", selection.Execution)
	}
	// The classifier runs in the mason's sandbox without a shell, so it is
	// not given the mason's Dagger engine or skills.
	classifier, err := turns.Select(context.Background(), coreadapter.Scope{Project: string(cfg.Project.ID), Role: "classifier"})
	must(t, err)
	if classifier.Execution.Mode != "sbx" || classifier.Execution.Dagger != nil || len(classifier.Execution.Skills) != 0 {
		t.Fatalf("classifier execution %+v", classifier.Execution)
	}
	if execution := threadExecution(masonRole, mason); execution.Mode != "sbx" || execution.Dagger == nil || *execution.Dagger != *mason.Dagger.Settings() || !slices.Equal(execution.Skills, mason.Skills) {
		t.Fatalf("mason execution %+v", execution)
	}
	if execution := threadExecution(reviewerRole, mason); execution.Dagger != nil {
		t.Fatalf("reviewer execution %+v", execution)
	}
	scope.Project = "other"
	if _, err := turns.Select(context.Background(), scope); err == nil {
		t.Fatal("selected a view of another project")
	}
}

// The skill cache refreshes as the loaded configuration says, and a reload
// changes the policy for the turns that start after it.
func TestSkillCacheFollowsLoadedRefreshPolicy(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	files := configFiles(t, opts)
	files.write(t, files.topText+"[skills]\nrefresh = \"always\"\n", files.projectText)
	cache := skills.NewManager(filepath.Join(opts.Config.Root, "skills"))
	opts.skills = cache
	_, c := start(t, opts)
	if cache.Refresh == nil || cache.Refresh() != "always" {
		t.Fatal("skill cache does not follow the loaded refresh policy")
	}
	files.write(t, files.topText+"[skills]\nrefresh = \"never\"\n", files.projectText)
	_, err := c.Reload(context.Background())
	must(t, err)
	if got := cache.Refresh(); got != "never" {
		t.Fatalf("refresh after reload %q", got)
	}
}
