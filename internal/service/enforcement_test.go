package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/projectstore"
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
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	chief := cfg.Roles[config.ManagerRole]
	chief.Sandbox, chief.Image, chief.Skills = "container", "loaded-image", []string{"https://github.com/acme/skills#skills/triage"}
	cfg.Roles[config.ManagerRole] = chief
	mason := cfg.Roles[masonRole]
	mason.Sandbox, mason.Skills = "sbx", []string{"https://github.com/acme/skills#skills/tdd"}
	cfg.Roles[masonRole] = mason
	cfg.Dagger = config.DaggerConfig{Version: "v0.20.5", Engine: "tcp://127.0.0.1:1234"}
	fixturekit.Must(t, os.WriteFile(filepath.Join(opts.Config.Root, "config.toml"), []byte("not toml ["), 0600))

	zone := time.FixedZone("east", 5*60*60)
	opts.Reconciliation.Now = func() time.Time { return time.Date(2026, 9, 17, 12, 0, 0, 0, zone) }
	repository, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	bound, err := Enforce(opts, Enforcement{}).Threads(repository, cfg)
	fixturekit.Must(t, err)
	dispatcher, ok := bound.(thread.Dispatcher)
	if !ok {
		t.Fatalf("threads reconciler %T", bound)
	}
	if now := dispatcher.Runner.Now(); now.Location() != time.UTC {
		t.Fatalf("thread clock %v is not UTC", now)
	}
	turns := dispatcher.Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
	scope := coreadapter.Scope{Project: string(cfg.Project.ID), Workstream: string(fixturekit.Stream), Role: trace.ChiefOfStaff}
	selection, err := turns.Select(context.Background(), scope)
	fixturekit.Must(t, err)
	if selection.Execution.Mode != "container" || selection.Execution.Image != "loaded-image" || selection.Execution.Dagger != nil || !slices.Equal(selection.Execution.Skills, chief.Skills) {
		t.Fatalf("execution %+v", selection.Execution)
	}
	if _, err := turns.Select(context.Background(), coreadapter.Scope{Project: string(cfg.Project.ID), Role: "classifier"}); err == nil {
		t.Fatal("selected a view for a classifier role")
	}
	if execution := cfg.RoleExecution(masonRole); execution.Mode != "sbx" || execution.Dagger == nil || *execution.Dagger != (agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}) || execution.DaggerCloudTokenEnv != "" || !slices.Equal(execution.Skills, mason.Skills) {
		t.Fatalf("mason execution %+v", execution)
	}
	if execution := cfg.RoleExecution(reviewerRole); execution.RunsDagger() {
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
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	files.Write(t, files.TopText+"[skills]\nrefresh = \"always\"\n")
	cache := skills.NewManager(filepath.Join(opts.Config.Root, "skills"))
	opts.skills = cache
	_, c := start(t, opts)
	if cache.Refresh == nil || cache.Refresh() != "always" {
		t.Fatal("skill cache does not follow the loaded refresh policy")
	}
	files.Write(t, files.TopText+"[skills]\nrefresh = \"never\"\n")
	_, err := c.Reload(context.Background())
	fixturekit.Must(t, err)
	if got := cache.Refresh(); got != "never" {
		t.Fatalf("refresh after reload %q", got)
	}
}

// Isolation serves a turn only the tools its role's grant names, so the
// chief of staff's grant names every tool a chief-of-staff turn is given.
func TestChiefGrantNamesEveryChiefTool(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "chief-grant")
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "turn"}
	tools, err := chiefTools(f.s.cfg, repository, &runtimeControls{}, scope, time.Now)
	fixturekit.Must(t, err)
	tools = append(tools, (&runtimeControls{}).preview(f.s.cfg, repository, scope, previewView(t), time.Now))
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
		if !slices.Contains(chiefGrant.Tools, tool.Name) {
			t.Errorf("chief-of-staff tool %s is not in the chief's grant", tool.Name)
		}
		if tool.Name == ProjectContextTool && tool.Effect != coreadapter.ToolRead {
			t.Errorf("project context effect %v, want read", tool.Effect)
		}
	}
	want := slices.DeleteFunc(ChiefOrdinaryToolsOf("feature"), func(name string) bool { return name == "file_read" })
	slices.Sort(want)
	registered := slices.Clone(names)
	slices.Sort(registered)
	if !slices.Equal(registered, want) {
		t.Errorf("ordinary chief handlers %v, want %v", registered, want)
	}
	bound, err := Enforce(f.s.options, Enforcement{}).Threads(repository, f.s.cfg)
	fixturekit.Must(t, err)
	turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
	wrapped, err := turns.Scoped(context.Background(), scope)
	fixturekit.Must(t, err)
	viewTools, err := turns.ViewTools(context.Background(), scope, previewView(t))
	fixturekit.Must(t, err)
	wrapped = append(wrapped, viewTools...)
	var wrappedNames []string
	for _, tool := range wrapped {
		wrappedNames = append(wrappedNames, tool.Name)
	}
	slices.Sort(wrappedNames)
	all := slices.DeleteFunc(slices.Clone(chiefGrant.Tools), func(name string) bool {
		return name == "file_read" || name == climbSteerTool || name == climbConcludeTool
	})
	slices.Sort(all)
	if !slices.Equal(wrappedNames, all) {
		t.Errorf("production chief registry %v, want %v", wrappedNames, all)
	}
	count := 0
	for _, name := range turns.Grants[trace.ChiefOfStaff].Tools {
		if name == ProjectContextTool {
			count++
		}
	}
	if count != 1 {
		t.Errorf("production chief project context grants: %d, want one", count)
	}
	for _, name := range []string{moveUnitTool, handBackDriftTool, resolveContestedTool, ProjectContextTool} {
		if !slices.Contains(names, name) {
			t.Errorf("a chief-of-staff turn is not given %s: %v", name, names)
		}
	}
}
