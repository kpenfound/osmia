package isolation

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest/enforcertest"
	"github.com/kpenfound/busybees/core/vcs"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/workspace"
)

// jujutsuWorkspaces lends one existing Jujutsu workspace to every turn.
type jujutsuWorkspaces struct{ w workspace.Worktree }

func (p jujutsuWorkspaces) Acquire(_ context.Context, req a.WorkspaceRequest) (a.WorkspaceLease, error) {
	return a.WorkspaceLease{Workspace: a.Workspace{Directory: p.w.Path, Access: req.Access}, Lease: leaseFunc(func(context.Context) error { return nil })}, nil
}

type fakeAgent func(context.Context, *enforcertest.Turn) (*agent.Result, error)

func (e fakeAgent) Enforcer(settings a.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image, Agent: e}, nil
}

func hostGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Owner", "GIT_AUTHOR_EMAIL=owner@example.invalid", "GIT_COMMITTER_NAME=Owner", "GIT_COMMITTER_EMAIL=owner@example.invalid"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A turn in a Jujutsu workspace works on a copy of its files alone: it holds
// no VCS access, cannot read or write the workspace's .jj, the repository
// behind it or the clone's Git metadata, cannot run jj or git, and the
// metadata it plants in its copy never reaches the workspace.
func TestTurnInAJujutsuWorkspaceGetsFilesOnly(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
			t.Fatalf("jj is required but not on PATH: %v", err)
		}
		t.Skip("jj is not installed")
	}
	for _, mode := range []string{agent.SandboxNone, agent.SandboxClaude, agent.SandboxContainer} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			home, err := os.MkdirTemp("", "jj-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			clone := filepath.Join(home, "clone")
			hostGit(t, "init", "--quiet", "-b", "main", clone)
			put(t, clone, "README", "widgets\n")
			put(t, clone, "src/main.go", "package main\n")
			hostGit(t, "-C", clone, "add", ".")
			hostGit(t, "-C", clone, "commit", "--quiet", "-m", "base")
			base := hostGit(t, "-C", clone, "rev-parse", "HEAD")
			j := &workspace.Jujutsu{Clone: clone, Directory: filepath.Join(home, "osmia", "branches", "p1")}
			acquired, err := j.Acquire(ctx, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
			if err != nil {
				t.Fatal(err)
			}
			w := acquired.(workspace.Worktree)
			if _, err := os.Stat(filepath.Join(w.Path, ".jj")); err != nil {
				t.Fatalf("the workspace has no .jj to hide: %v", err)
			}
			// The metadata the workspace's own access names is what a turn must not reach.
			metadata := append(slices.Clone(w.VCS().Mounts), filepath.Join(w.Path, ".jj"))
			for _, mount := range w.VCS().Mounts {
				metadata = append(metadata, filepath.Join(mount, "config"), filepath.Join(mount, "HEAD"))
			}
			metadata = append(metadata, filepath.Join(w.Path, ".jj", "working_copy", "tree_state"), filepath.Join(clone, ".git", "config"))

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
				for _, name := range []string{".jj", ".git"} {
					if _, err := os.Lstat(filepath.Join(view, name)); !errors.Is(err, fs.ErrNotExist) {
						problems = append(problems, "the view holds "+name)
					}
				}
				data, err := turn.ReadFile(filepath.Join(view, "README"))
				problem("reading the view", err)
				if string(data) != "widgets\n" {
					problems = append(problems, "the view's README is "+string(data))
				}
				problem("writing the view", turn.WriteFile(filepath.Join(view, "README"), []byte("widgets, built\n")))
				for _, path := range append(slices.Clone(metadata), filepath.Join(w.Path, "README")) {
					if _, err := turn.ReadFile(path); !errors.Is(err, fs.ErrPermission) {
						problems = append(problems, "read "+path)
					}
					if err := turn.WriteFile(path, []byte("x")); !errors.Is(err, fs.ErrPermission) {
						problems = append(problems, "wrote "+path)
					}
				}
				for _, name := range []string{"jj", "/usr/bin/jj", "/usr/local/bin/jj", "git", "gh"} {
					if err := turn.Exec(name); !errors.Is(err, fs.ErrPermission) {
						problems = append(problems, "ran "+name)
					}
				}
				put(t, view, ".jj/repo/config", "planted")
				put(t, view, "src/.git/config", "[core]\n")
				return &agent.Result{ResultText: "built"}, nil
			}
			execution := a.ExecutionSettings{Mode: mode}
			if mode == agent.SandboxContainer {
				execution.Image = "fixture-image"
			}
			var captured string
			turns := &Turns{
				Workspaces: jujutsuWorkspaces{w},
				Views:      Views{Directory: filepath.Join(home, "views")},
				Select: func(context.Context, a.Scope) (Selection, error) {
					return Selection{Paths: []string{"README", "src"}, Execution: execution}, nil
				},
				Grants: map[string]a.Capabilities{"mason": {WriteFiles: true, Execute: true}},
				Engine: fakeAgent(mason),
				Capture: func(_ context.Context, _ a.Scope, view *FileView, _ a.SessionResult) error {
					data, err := view.Read("README")
					captured = string(data)
					return err
				},
			}
			if err := os.MkdirAll(turns.Views.Directory, 0700); err != nil {
				t.Fatal(err)
			}
			result, err := turns.Run(ctx, a.PreparedTurn{Scope: a.Scope{Role: "mason", Turn: "turn-1"}, Profile: a.Profile{Backend: agent.AgentClaude},
				SessionDirectory: filepath.Join(home, "session"), Prompt: "Build"})
			if err != nil || result.FinalResponse != "built" {
				t.Fatalf("the turn: %+v %v", result, err)
			}
			if len(problems) != 0 {
				t.Fatalf("the turn could:\n%s", strings.Join(problems, "\n"))
			}
			if captured != "widgets, built\n" {
				t.Fatalf("captured %q", captured)
			}
			if data, err := os.ReadFile(filepath.Join(w.Path, "README")); err != nil || string(data) != "widgets\n" {
				t.Fatalf("the workspace's README after the turn: %q %v", data, err)
			}
			if _, err := os.Lstat(filepath.Join(w.Path, "src", ".git")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("planted metadata in the workspace: %v", err)
			}
			if _, err := os.Stat(filepath.Join(w.Path, ".jj", "repo", "config")); err == nil {
				t.Fatal("planted .jj file in the workspace")
			}
		})
	}
}
