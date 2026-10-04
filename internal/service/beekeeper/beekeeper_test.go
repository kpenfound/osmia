package beekeeper

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	projectA config.ProjectID    = "p_a1111111111111111111111111111111"
	projectB config.ProjectID    = "p_b2222222222222222222222222222222"
	streamA  config.WorkstreamID = "w_a1111111111111111111111111111111"
	streamB  config.WorkstreamID = "w_b2222222222222222222222222222222"
)

var at = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
var owner = trace.Actor{Kind: "owner", ID: "local"}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0700))
	cmd := exec.Command("git", "init", "--quiet", dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

// snapshot reads every file below dir, including VCS metadata.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}))
	return files
}

// Open builds the shadow project's config.Project from the Beekeeper
// section, with no target repository, creates it on first call and reopens
// it, with its one reserved workstream, on a later call against the same
// root, without duplicating the workstream.
func TestOpenCreatesThenReopens(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	b := config.Beekeeper{Name: "Hive", Profile: "default", Sandbox: "none"}

	p := Project(b)
	if p.ID != config.ShadowProjectID || p.Name != "Hive" || p.Clone != "" {
		t.Fatalf("shadow project: %+v", p)
	}

	repo, err := Open(ctx, root, b, at)
	must(t, err)
	streams, err := repo.Workstreams()
	must(t, err)
	if !reflect.DeepEqual(streams, []config.WorkstreamID{config.BeekeeperWorkstreamID}) {
		t.Fatalf("workstreams: %v", streams)
	}
	directory, err := root.ProjectTrace(config.ShadowProjectID)
	must(t, err)
	if _, err := os.Stat(filepath.Join(directory, ".git")); err != nil {
		t.Fatalf("shadow trace not under the service state directory: %v", err)
	}
	entries, err := os.ReadDir(base)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "osmia" {
		t.Fatalf("unexpected target repository cloned beside the shadow trace: %v", entries)
	}
	before := snapshot(t, directory)
	must(t, repo.Close())

	reopened, err := Open(ctx, root, b, at.Add(time.Hour))
	must(t, err)
	defer reopened.Close()
	if !reflect.DeepEqual(snapshot(t, directory), before) {
		t.Fatal("reopening the shadow project changed its trace")
	}
	streams, err = reopened.Workstreams()
	must(t, err)
	if !reflect.DeepEqual(streams, []config.WorkstreamID{config.BeekeeperWorkstreamID}) {
		t.Fatalf("workstreams after reopen: %v", streams)
	}
}

// Creating the shadow project leaves every registered project's
// configuration, trace and workstreams byte-for-byte unchanged, and the
// owner's project configuration never mentions it.
func TestOpenLeavesRegisteredProjectsUnchanged(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	topPath, err := root.Config()
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(topPath), 0700))
	must(t, os.WriteFile(topPath, []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))

	for _, p := range []struct {
		id     config.ProjectID
		stream config.WorkstreamID
		name   string
	}{{projectA, streamA, "alpha"}, {projectB, streamB, "beta"}} {
		clone := filepath.Join(base, string(p.id))
		gitInit(t, clone)
		projectPath, err := root.ProjectConfig(p.id)
		must(t, err)
		must(t, config.WriteProjectConfig(projectPath, config.Project{Name: p.name, Upstream: "owner/" + p.name, Clone: clone, BaseBranch: "main"}))
		must(t, config.AddActiveProject(topPath, p.id))
		repo, err := trace.Create(ctx, root, config.Project{ID: p.id, Clone: clone}, at, owner)
		must(t, err)
		must(t, repo.CreateWorkstream(ctx, p.stream, at, owner))
		must(t, repo.Close())
	}
	cfg, err := config.Load(config.Options{Root: root.String()})
	must(t, err)
	if len(cfg.Projects) != 2 {
		t.Fatalf("expected two registered projects: %+v", cfg.Projects)
	}
	beforeTop, err := os.ReadFile(topPath)
	must(t, err)
	if strings.Contains(string(beforeTop), string(config.ShadowProjectID)) {
		t.Fatal("owner's project configuration already mentions the shadow project")
	}
	type snap struct {
		configText string
		trace      map[string]string
		streams    []config.WorkstreamID
	}
	before := map[config.ProjectID]snap{}
	for _, p := range cfg.Projects {
		directory, err := root.ProjectTrace(p.ID)
		must(t, err)
		projectPath, err := root.ProjectConfig(p.ID)
		must(t, err)
		text, err := os.ReadFile(projectPath)
		must(t, err)
		repo, err := trace.Open(root, p)
		must(t, err)
		streams, err := repo.Workstreams()
		must(t, err)
		must(t, repo.Close())
		before[p.ID] = snap{string(text), snapshot(t, directory), streams}
	}

	shadow, err := Open(ctx, root, cfg.Beekeeper, time.Now().UTC())
	must(t, err)
	must(t, shadow.Close())

	afterTop, err := os.ReadFile(topPath)
	must(t, err)
	if string(afterTop) != string(beforeTop) {
		t.Fatalf("top-level configuration changed:\n%s", afterTop)
	}
	if strings.Contains(string(afterTop), string(config.ShadowProjectID)) {
		t.Fatal("owner's project configuration mentions the shadow project")
	}
	for _, p := range cfg.Projects {
		directory, err := root.ProjectTrace(p.ID)
		must(t, err)
		projectPath, err := root.ProjectConfig(p.ID)
		must(t, err)
		text, err := os.ReadFile(projectPath)
		must(t, err)
		repo, err := trace.Open(root, p)
		must(t, err)
		streams, err := repo.Workstreams()
		must(t, err)
		must(t, repo.Close())
		want := before[p.ID]
		if string(text) != want.configText {
			t.Fatalf("project %s configuration changed", p.ID)
		}
		if !reflect.DeepEqual(snapshot(t, directory), want.trace) {
			t.Fatalf("project %s trace changed", p.ID)
		}
		if !reflect.DeepEqual(streams, want.streams) {
			t.Fatalf("project %s workstreams changed: %v vs %v", p.ID, streams, want.streams)
		}
	}
}
