package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
)

// unitsFixture is a service configuration whose clone holds the feature
// branch of stream, one commit with README, LICENSE, bin/run and docs/guide.
type unitsFixture struct {
	cfg        *config.Config
	home, base string
}

func newUnitsFixture(t *testing.T) unitsFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "uw-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.Load(fixtureAt(t, home).Config)
	must(t, err)
	clone := cfg.Project.Clone
	demoGit(t, home, "init", "--quiet", "-b", "main", clone)
	must(t, os.WriteFile(filepath.Join(clone, "README"), []byte("widgets\n"), 0644))
	must(t, os.MkdirAll(filepath.Join(clone, "bin"), 0755))
	must(t, os.WriteFile(filepath.Join(clone, "bin", "run"), []byte("#!/bin/sh\n"), 0755))
	must(t, os.WriteFile(filepath.Join(clone, "LICENSE"), []byte("MIT\n"), 0644))
	must(t, os.MkdirAll(filepath.Join(clone, "docs"), 0755))
	must(t, os.WriteFile(filepath.Join(clone, "docs", "guide"), []byte("guide\n"), 0644))
	demoGit(t, home, "-C", clone, "add", "README", "bin/run", "LICENSE", "docs/guide")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "--quiet", "-m", "base")
	demoGit(t, home, "-C", clone, "branch", featureBranch(stream))
	return unitsFixture{cfg: cfg, home: home, base: strings.TrimSpace(demoGit(t, home, "-C", clone, "rev-parse", "HEAD"))}
}

// units is a new service's view of the unit workspaces, as after a restart.
func (f unitsFixture) units() unitWorkspaces { return (&Service{cfg: f.cfg}).unitWorkspaces() }

// A unit's workspace is created once from the feature branch, under the
// root, and a service built afresh finds the same one with the same base. It
// keeps its work: finding it again, opening it again and pruning leave it as
// it is.
func TestUnitWorkspaceIsFoundAgainAfterARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	if _, _, found, err := units.find(ctx, stream, "u1"); err != nil || found {
		t.Fatalf("a unit workspace before it is opened: %v %v", found, err)
	}
	w, base, err := units.open(ctx, stream, "u1")
	must(t, err)
	want := filepath.Join(f.cfg.Root.String(), unitsDirectory, string(project), string(stream), "u1")
	if w.Path != want || w.Branch != "osmia-unit/"+string(stream)+"/u1" || base != f.base {
		t.Fatalf("opened %+v at base %s; want %s on %s at %s", w, base, want, unitBranch(stream, "u1"), f.base)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "README")); err != nil || string(data) != "widgets\n" {
		t.Fatalf("the workspace's files: %q %v", data, err)
	}
	must(t, os.WriteFile(filepath.Join(w.Path, "work"), []byte("in progress\n"), 0644))
	// The feature branch and the unit's branch both move on: the base stays
	// the commit the workspace was created from.
	candidate, err := units.snapshot(ctx, stream, "u1")
	must(t, err)
	clone := f.cfg.Project.Clone
	demoGit(t, f.home, "-C", clone, "checkout", "--quiet", featureBranch(stream))
	must(t, os.WriteFile(filepath.Join(clone, "NEWS"), []byte("later\n"), 0644))
	demoGit(t, f.home, "-C", clone, "add", "NEWS")
	demoGit(t, f.home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "--quiet", "-m", "later")
	if tip, _, err := units.git.Branch(ctx, featureBranch(stream)); err != nil || tip == f.base || candidate == f.base {
		t.Fatalf("the feature branch at %s and the candidate %s have not moved from %s: %v", tip, candidate, f.base, err)
	}

	restarted := f.units()
	found, foundBase, ok, err := restarted.find(ctx, stream, "u1")
	if err != nil || !ok || found.Path != w.Path || found.Branch != w.Branch || foundBase != f.base {
		t.Fatalf("found again %+v at %s, %v %v; want %+v at %s", found, foundBase, ok, err, w, f.base)
	}
	reopened, reopenedBase, err := restarted.open(ctx, stream, "u1")
	if err != nil || reopened.Path != w.Path || reopenedBase != f.base {
		t.Fatalf("opened again %+v at %s, %v", reopened, reopenedBase, err)
	}
	must(t, restarted.git.Prune(ctx))
	if data, err := os.ReadFile(filepath.Join(w.Path, "work")); err != nil || string(data) != "in progress\n" {
		t.Fatalf("the unit's work after finding, opening and pruning: %q %v", data, err)
	}
	if other, _, err := restarted.open(ctx, stream, "u2"); err != nil || other.Path == w.Path {
		t.Fatalf("another unit's workspace: %+v %v", other, err)
	}
	if _, _, err := restarted.open(ctx, "w_ffffffffffffffffffffffffffffffff", "u1"); err == nil || !strings.Contains(err.Error(), "the clone has no feature branch osmia/w_ffffffffffffffffffffffffffffffff") {
		t.Fatalf("a unit of a workstream without a feature branch: %v", err)
	}
}

// The candidate of a unit is a snapshot of its workspace: a commit holding
// exactly the workspace's changes that descends from the feature branch.
func TestUnitSnapshotIsACandidateOnTheFeatureBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	if _, err := units.snapshot(ctx, stream, "u1"); err == nil || !strings.Contains(err.Error(), "unit u1 of workstream "+string(stream)+" has no workspace") {
		t.Fatalf("a snapshot of a unit without a workspace: %v", err)
	}
	w, _, err := units.open(ctx, stream, "u1")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, "README"), []byte("widgets, built\n"), 0644))
	must(t, os.WriteFile(filepath.Join(w.Path, "added"), []byte("new\n"), 0644))
	candidate, err := f.units().snapshot(ctx, stream, "u1")
	must(t, err)
	if got := strings.TrimSpace(demoGit(t, f.home, "-C", f.cfg.Project.Clone, "diff-tree", "-r", "--name-status", f.base, candidate)); got != "M\tREADME\nA\tadded" {
		t.Fatalf("the candidate's changes:\n%s", got)
	}
	if descends, err := units.git.Ancestor(ctx, featureBranch(stream), candidate); err != nil || !descends {
		t.Fatalf("the candidate descends from the feature branch: %v %v", descends, err)
	}
}

// masonEngine hands out core's fake enforcer with the given agent.
type masonEngine func(context.Context, *enforcertest.Turn) (*agent.Result, error)

func (e masonEngine) Enforcer(settings coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image, Agent: e}, nil
}

// masonTurn runs a mason turn of unit u1 through isolation.Turns the way the
// service lends it its unit's workspace, with mason playing the agent.
func (f unitsFixture) masonTurn(ctx context.Context, t *testing.T, units unitWorkspaces, execution coreadapter.ExecutionSettings, mason masonEngine) (coreadapter.SessionResult, error) {
	t.Helper()
	turns := &isolation.Turns{
		Workspaces: units,
		Views:      isolation.Views{Directory: filepath.Join(f.cfg.Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			w, _, _, err := units.find(ctx, config.WorkstreamID(scope.Workstream), scope.Unit)
			if err != nil {
				return isolation.Selection{}, err
			}
			paths, err := units.paths(w)
			return isolation.Selection{Paths: paths, Execution: execution}, err
		},
		Grants:  map[string]coreadapter.Capabilities{masonRole: {WriteFiles: true, Execute: true}},
		Engine:  mason,
		Capture: units.capture,
	}
	must(t, os.MkdirAll(turns.Views.Directory, 0700))
	input := coreadapter.PreparedTurn{Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: "u1", Thread: "mason-u1", Turn: "turn-1", Role: masonRole},
		Profile: coreadapter.Profile{Backend: agent.AgentClaude}, SessionDirectory: filepath.Join(f.home, "session"), Prompt: "Build u1"}
	return turns.Run(ctx, input)
}

// A mason turn in a unit's workspace works on a copy of its files alone: it
// can neither read nor write the clone's or the worktree's VCS metadata, has
// no VCS executable and none of the host's credentials, and VCS metadata or a
// symlink it plants in its copy never reaches the workspace. What it wrote,
// removed, turned from a file into a directory or back, and made executable
// or not is in the workspace after the turn, ready to be snapshotted.
func TestMasonTurnGetsTheUnitWorkspaceFilesOnly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_host")
	t.Setenv("GH_TOKEN", "ghp_host")
	for _, mode := range []string{agent.SandboxNone, agent.SandboxClaude, agent.SandboxContainer} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newUnitsFixture(t)
			units := f.units()
			w, _, err := units.open(ctx, stream, "u1")
			must(t, err)
			clone := f.cfg.Project.Clone
			dotgit := filepath.Join(w.Path, ".git")
			pointer, err := os.ReadFile(dotgit)
			must(t, err)
			metadata := filepath.Join(strings.TrimSpace(strings.TrimPrefix(string(pointer), "gitdir:")), "HEAD")
			var problems []string
			problem := func(what string, err error) {
				if err != nil {
					problems = append(problems, what+": "+err.Error())
				}
			}
			mason := func(_ context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
				view := turn.Request.Workspace.Directory()
				if view == w.Path || turn.Request.Workspace.VCS() != nil || turn.Request.Profile.VCSAccess || turn.Policy.VCS {
					problems = append(problems, "the turn holds the workspace or its VCS")
				}
				for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
					if _, ok := turn.Request.Env[name]; ok || slices.Contains(turn.Policy.Env, name) {
						problems = append(problems, "the turn's environment holds "+name)
					}
				}
				if _, err := os.Lstat(filepath.Join(view, ".git")); !errors.Is(err, fs.ErrNotExist) {
					problems = append(problems, "the view holds .git")
				}
				data, err := turn.ReadFile(filepath.Join(view, "README"))
				problem("reading the view", err)
				if string(data) != "widgets\n" {
					problems = append(problems, "the view's README is "+string(data))
				}
				problem("writing the view", turn.WriteFile(filepath.Join(view, "README"), []byte("widgets, built\n")))
				must(t, os.MkdirAll(filepath.Join(view, "src"), 0700))
				problem("adding to the view", turn.WriteFile(filepath.Join(view, "src", "added.go"), []byte("package src\n")))
				for _, path := range []string{dotgit, metadata, filepath.Join(clone, ".git", "config"), filepath.Join(w.Path, "README")} {
					if _, err := turn.ReadFile(path); !errors.Is(err, fs.ErrPermission) {
						problems = append(problems, "read "+path)
					}
					if err := turn.WriteFile(path, []byte("x")); !errors.Is(err, fs.ErrPermission) {
						problems = append(problems, "wrote "+path)
					}
				}
				for _, name := range []string{"git", "/usr/bin/git", "gh"} {
					if err := turn.Exec(name); !errors.Is(err, fs.ErrPermission) {
						problems = append(problems, "ran "+name)
					}
				}
				// What the turn may do in its own copy: plant metadata and a
				// link to the clone's.
				must(t, os.WriteFile(filepath.Join(view, ".git"), []byte("gitdir: /elsewhere\n"), 0600))
				must(t, os.MkdirAll(filepath.Join(view, "src", ".git"), 0700))
				must(t, os.WriteFile(filepath.Join(view, "src", ".git", "config"), []byte("[core]\n"), 0600))
				must(t, os.Symlink(filepath.Join(clone, ".git"), filepath.Join(view, "escape")))
				must(t, os.Chmod(filepath.Join(view, "bin", "run"), 0600))
				must(t, os.MkdirAll(filepath.Join(view, "tools"), 0700))
				must(t, os.WriteFile(filepath.Join(view, "tools", "build"), []byte("#!/bin/sh\n"), 0700))
				// A file becomes a directory and a directory a file.
				must(t, os.Remove(filepath.Join(view, "LICENSE")))
				must(t, os.MkdirAll(filepath.Join(view, "LICENSE"), 0700))
				must(t, os.WriteFile(filepath.Join(view, "LICENSE", "MIT"), []byte("MIT\n"), 0600))
				must(t, os.RemoveAll(filepath.Join(view, "docs")))
				must(t, os.WriteFile(filepath.Join(view, "docs"), []byte("see the wiki\n"), 0600))
				return &agent.Result{ResultText: "built"}, nil
			}
			execution := coreadapter.ExecutionSettings{Mode: mode}
			if mode == agent.SandboxContainer {
				execution.Image = "fixture-image"
			}
			result, err := f.masonTurn(ctx, t, units, execution, mason)
			if err != nil || result.FinalResponse != "built" {
				t.Fatalf("the mason turn: %+v %v", result, err)
			}
			if len(problems) != 0 {
				t.Fatalf("the mason turn could:\n%s", strings.Join(problems, "\n"))
			}
			if after, err := os.ReadFile(dotgit); err != nil || string(after) != string(pointer) {
				t.Fatalf("the workspace's .git after the turn: %q %v", after, err)
			}
			for _, name := range []string{"escape", "src/.git", "docs/guide"} {
				if _, err := os.Lstat(filepath.Join(w.Path, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("%s in the workspace after the turn: %v", name, err)
				}
			}
			candidate, err := units.snapshot(ctx, stream, "u1")
			must(t, err)
			changes := strings.Split(strings.TrimSpace(demoGit(t, f.home, "-C", clone, "diff-tree", "-r", "--name-status", "--no-renames", f.base, candidate)), "\n")
			slices.Sort(changes)
			if want := []string{"A\tLICENSE/MIT", "A\tdocs", "A\tsrc/added.go", "A\ttools/build", "D\tLICENSE", "D\tdocs/guide", "M\tREADME", "M\tbin/run"}; !slices.Equal(changes, want) {
				t.Fatalf("the candidate after the turn:\n%s\nwant\n%s", strings.Join(changes, "\n"), strings.Join(want, "\n"))
			}
			for name, want := range map[string]string{"bin/run": "100644", "tools/build": "100755", "README": "100644", "docs": "100644"} {
				if got := strings.Fields(demoGit(t, f.home, "-C", clone, "ls-tree", candidate, name))[0]; got != want {
					t.Fatalf("%s is %s in the candidate, want %s", name, got, want)
				}
			}
		})
	}
}

// A unit workspace is lent to a mason turn of a unit whose workspace exists,
// and to no other turn.
func TestUnitWorkspaceIsLentToItsMasonAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	w, _, err := units.open(ctx, stream, "u1")
	must(t, err)
	mason := coreadapter.Scope{Workstream: string(stream), Unit: "u1", Role: masonRole}
	lease, err := units.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: mason, Access: coreadapter.ReadWrite})
	if err != nil || lease.Workspace.Directory != w.Path || lease.Workspace.Access != coreadapter.ReadWrite {
		t.Fatalf("the mason's lease: %+v %v", lease, err)
	}
	must(t, lease.Lease.Release(ctx))
	if _, err := os.Stat(filepath.Join(w.Path, "README")); err != nil {
		t.Fatalf("the workspace after its lease is released: %v", err)
	}
	for _, scope := range []coreadapter.Scope{
		{Workstream: string(stream), Unit: "u1", Role: "reviewer"},
		{Workstream: string(stream), Role: masonRole},
		{Unit: "u1", Role: masonRole},
	} {
		if _, err := units.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: scope, Access: coreadapter.ReadWrite}); err == nil || err.Error() != "a unit workspace is lent to a mason turn of a unit alone" {
			t.Fatalf("lending to %+v: %v", scope, err)
		}
	}
	missing := coreadapter.Scope{Workstream: string(stream), Unit: "u2", Role: masonRole}
	if _, err := units.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: missing, Access: coreadapter.ReadWrite}); err == nil || err.Error() != "unit u2 of workstream "+string(stream)+" has no workspace" {
		t.Fatalf("lending a workspace never opened: %v", err)
	}
}

// A workspace whose tree holds a symlink cannot be lent to a mason turn: the
// turn fails before it runs, and the workspace keeps the link.
func TestMasonTurnIsRefusedAWorkspaceHoldingASymlink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	w, _, err := units.open(ctx, stream, "u1")
	must(t, err)
	must(t, os.Symlink("../README", filepath.Join(w.Path, "docs", "readme")))
	ran := false
	_, err = f.masonTurn(ctx, t, units, coreadapter.ExecutionSettings{Mode: agent.SandboxNone}, func(context.Context, *enforcertest.Turn) (*agent.Result, error) {
		ran = true
		return &agent.Result{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "symlinks and special files are not exposed") || ran {
		t.Fatalf("a mason turn in a workspace holding a symlink: ran %v, %v", ran, err)
	}
	if target, err := os.Readlink(filepath.Join(w.Path, "docs", "readme")); err != nil || target != "../README" {
		t.Fatalf("the workspace's link after the refused turn: %q %v", target, err)
	}
}
