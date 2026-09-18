// Package workspace creates and inspects the service's workspaces on a
// project's clone. The implementation is Git worktrees behind core's workspace
// provider interface; the service alone runs it, and no agent holds it.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kpenfound/busybees/core/vcs"
)

// Git is the worktree provider of one clone. Every workspace is a worktree of
// Clone under Directory, named by the request, on the branch the request
// names.
type Git struct {
	Clone     string
	Directory string
}

var _ vcs.Provider = (*Git)(nil)

// Worktree is one workspace: a linked worktree whose repository metadata is
// the clone's.
type Worktree struct {
	Path   string
	Branch string
	git    string
}

func (w Worktree) Directory() string { return w.Path }
func (w Worktree) VCS() *vcs.Access  { return &vcs.Access{Mounts: []string{w.git}} }

// Remote returns the name of the clone's remote whose URL names the given
// owner/repository, however the URL spells it: HTTPS, SSH, with or without a
// .git suffix, or a local path. The first configured one wins.
func (g *Git) Remote(ctx context.Context, repository string) (string, error) {
	out, err := g.run(ctx, "config", "--get-regexp", `^remote\..*\.url$`)
	if err != nil && !exitCode(err, 1) {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		key, url, ok := strings.Cut(line, " ")
		if !ok || !names(url, repository) {
			continue
		}
		return strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url"), nil
	}
	return "", fmt.Errorf("the clone has no remote whose URL names %s", repository)
}

// names reports whether a remote URL names owner/repository.
func names(url, repository string) bool {
	url = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git")
	return strings.HasSuffix(strings.ToLower(url), strings.ToLower("/"+repository)) || strings.HasSuffix(strings.ToLower(url), strings.ToLower(":"+repository))
}

// Fetch fetches one branch of a remote into the clone's remote-tracking ref
// and returns the commit it points at. SSH runs in batch mode unless the
// owner configured an SSH command of their own, so a passphrase or an unknown
// host key fails the fetch instead of waiting for a terminal.
func (g *Git) Fetch(ctx context.Context, remote, branch string) (string, error) {
	env, err := g.sshEnvironment(ctx)
	if err != nil {
		return "", err
	}
	ref := "refs/remotes/" + remote + "/" + branch
	if _, err := g.runEnv(ctx, env, "fetch", "--quiet", "--no-tags", remote, "+refs/heads/"+branch+":"+ref); err != nil {
		return "", err
	}
	commit, err := g.run(ctx, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return commit, nil
}

// Branch returns the commit a local branch of the clone points at, and whether
// the branch exists.
func (g *Git) Branch(ctx context.Context, name string) (string, bool, error) {
	out, err := g.run(ctx, "for-each-ref", "--format=%(objectname)", "refs/heads/"+name)
	if err != nil || out == "" {
		return "", false, err
	}
	return out, true, nil
}

// Ancestor reports whether commit is reachable from tip.
func (g *Git) Ancestor(ctx context.Context, commit, tip string) (bool, error) {
	_, err := g.run(ctx, "merge-base", "--is-ancestor", commit, tip)
	if exitCode(err, 1) {
		return false, nil
	}
	return err == nil, err
}

// MergeBase returns the best common ancestor of two commits.
func (g *Git) MergeBase(ctx context.Context, a, b string) (string, error) {
	return g.run(ctx, "merge-base", a, b)
}

// identity is the author and committer of the commits the service makes.
var identity = []string{"GIT_AUTHOR_NAME=Osmia", "GIT_AUTHOR_EMAIL=osmia@localhost", "GIT_COMMITTER_NAME=Osmia", "GIT_COMMITTER_EMAIL=osmia@localhost"}

// Snapshot commits the worktree's full tree, untracked files included and
// files the repository ignores left out, on top of the commit the worktree is
// on, moves the worktree's branch to it and returns it. The owner's own
// excludes file does not apply, so what the snapshot holds depends on the
// repository alone. A worktree whose tree is the one of its commit is not
// committed again: its commit is the snapshot. The worktree must descend from
// base, which is checked before anything is committed.
func (g *Git) Snapshot(ctx context.Context, w Worktree, base string) (string, error) {
	if w.Path == "" {
		return "", errors.New("no workspace to snapshot")
	}
	head, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	descends, err := g.Ancestor(ctx, base, head)
	if err != nil {
		return "", err
	}
	if !descends {
		return "", fmt.Errorf("workspace %s is at %s, which does not descend from %s", w.Path, head, base)
	}
	if _, err := g.runIn(ctx, w.Path, nil, "-c", "core.excludesFile="+os.DevNull, "add", "--all"); err != nil {
		return "", err
	}
	tree, err := g.runIn(ctx, w.Path, nil, "write-tree")
	if err != nil {
		return "", err
	}
	current, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if tree == current {
		return head, nil
	}
	commit, err := g.runIn(ctx, w.Path, identity, "commit-tree", "--no-gpg-sign", "-p", head, "-m", "Snapshot of "+w.Branch, tree)
	if err != nil {
		return "", err
	}
	if _, err := g.runIn(ctx, w.Path, identity, "update-ref", "-m", "osmia: snapshot", "HEAD", commit, head); err != nil {
		return "", err
	}
	return commit, nil
}

// listed is one entry of the clone's worktree list.
type listed struct {
	path, branch string
	prunable     bool
}

// list reads the clone's worktrees.
func (g *Git) list(ctx context.Context) ([]listed, error) {
	out, err := g.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []listed
	for _, block := range strings.Split(out, "\n\n") {
		var entry listed
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				entry.path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch refs/heads/"):
				entry.branch = strings.TrimPrefix(line, "branch refs/heads/")
			case line == "prunable" || strings.HasPrefix(line, "prunable "):
				entry.prunable = true
			}
		}
		if entry.path != "" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// own reports whether a listed worktree is the one named name: Git lists the
// directory it resolved when the worktree was added.
func (g *Git) own(entry listed, name string) bool {
	path := g.path(name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	return entry.path == path || entry.path == resolved
}

// Workspace returns the worktree named name when the clone has one. One
// whose directory is gone is forgotten first, so it can be made again.
func (g *Git) Workspace(ctx context.Context, name string) (Worktree, bool, error) {
	entries, err := g.list(ctx)
	if err != nil {
		return Worktree{}, false, err
	}
	for _, entry := range entries {
		if !g.own(entry, name) {
			continue
		}
		if entry.prunable {
			return Worktree{}, false, g.forget(ctx, entry)
		}
		return Worktree{Path: g.path(name), Branch: entry.branch, git: filepath.Join(g.Clone, ".git")}, true, nil
	}
	return Worktree{}, false, nil
}

// forget removes one worktree's metadata from the clone.
func (g *Git) forget(ctx context.Context, entry listed) error {
	_, err := g.run(ctx, "worktree", "remove", "--force", entry.path)
	return err
}

// Acquire returns the worktree named by the request on its branch, creating
// what is missing: the branch from the request's ref when the clone has no
// such branch, and the worktree on the branch when the clone has no such
// worktree. A worktree already there is returned as it is when it is on the
// branch, so a repeated request creates nothing twice. The request needs a
// branch; a directory in the way that is no worktree of the clone is left
// alone and reported.
func (g *Git) Acquire(ctx context.Context, req vcs.Request) (vcs.Workspace, error) {
	if req.Name == "" || req.Branch == "" {
		return nil, errors.New("a workspace request needs a name and a branch")
	}
	existing, found, err := g.Workspace(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if found {
		if existing.Branch != req.Branch {
			return nil, fmt.Errorf("workspace %s is on branch %q, not %q", existing.Path, existing.Branch, req.Branch)
		}
		return existing, nil
	}
	path := g.path(req.Name)
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s exists and is no workspace of the clone; move it away", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_, exists, err := g.Branch(ctx, req.Branch)
	if err != nil {
		return nil, err
	}
	args := []string{"worktree", "add", "--quiet"}
	if exists {
		args = append(args, path, req.Branch)
	} else {
		if req.Ref == "" {
			return nil, fmt.Errorf("branch %s does not exist and the request names no ref to create it from", req.Branch)
		}
		args = append(args, "-b", req.Branch, path, req.Ref)
	}
	if _, err := g.run(ctx, args...); err != nil {
		return nil, err
	}
	return Worktree{Path: path, Branch: req.Branch, git: filepath.Join(g.Clone, ".git")}, nil
}

// Release removes the worktree, whatever it holds; the branch stays.
func (g *Git) Release(ctx context.Context, w vcs.Workspace) error {
	if w == nil || w.Directory() == "" {
		return errors.New("no workspace to release")
	}
	_, err := g.run(ctx, "worktree", "remove", "--force", w.Directory())
	return err
}

// Prune forgets the provider's own worktrees, those under Directory, whose
// directories are gone. The owner's other worktrees are left alone.
func (g *Git) Prune(ctx context.Context) error {
	entries, err := g.list(ctx)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(g.Directory)
	if err != nil {
		resolved = g.Directory
	}
	for _, entry := range entries {
		if !entry.prunable {
			continue
		}
		if rel, err := filepath.Rel(resolved, entry.path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if err := g.forget(ctx, entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *Git) path(name string) string { return filepath.Join(g.Directory, name) }

// sshEnvironment returns the environment that puts SSH in batch mode, or
// nothing when the owner has an SSH command of their own: GIT_SSH_COMMAND or
// GIT_SSH in the environment, or core.sshCommand in their Git configuration.
func (g *Git) sshEnvironment(ctx context.Context) ([]string, error) {
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return nil, nil
	}
	out, err := g.run(ctx, "config", "--get", "core.sshCommand")
	if err != nil && !exitCode(err, 1) {
		return nil, err
	}
	if out != "" {
		return nil, nil
	}
	return []string{"GIT_SSH_COMMAND=ssh -o BatchMode=yes"}, nil
}

// run runs Git in the clone with hooks disabled and no prompt. The owner's
// Git configuration, agent and SSH command stay in reach, because fetching
// the owner's remotes may need their credentials.
func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	return g.runEnv(ctx, nil, args...)
}

func (g *Git) runEnv(ctx context.Context, extra []string, args ...string) (string, error) {
	return g.runIn(ctx, g.Clone, extra, args...)
}

// runIn runs Git the way run does, in dir rather than the clone.
func (g *Git) runIn(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Env = []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	for _, name := range []string{"PATH", "HOME", "SSH_AUTH_SOCK", "GIT_SSH_COMMAND", "GIT_SSH", "XDG_CONFIG_HOME", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctxErr)
		}
		return "", &Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Error is a Git command that failed, with what it said.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("git %s: %v", e.Args[0], e.Err)
	}
	return fmt.Sprintf("git %s: %v: %s", e.Args[0], e.Err, e.Stderr)
}
func (e *Error) Unwrap() error { return e.Err }

// exitCode reports whether err is a Git command that exited with code.
func exitCode(err error, code int) bool {
	var e *Error
	var exit *exec.ExitError
	return errors.As(err, &e) && errors.As(e.Err, &exit) && exit.ExitCode() == code
}
