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
// and returns the commit it points at.
func (g *Git) Fetch(ctx context.Context, remote, branch string) (string, error) {
	ref := "refs/remotes/" + remote + "/" + branch
	if _, err := g.run(ctx, "fetch", "--quiet", "--no-tags", remote, "+refs/heads/"+branch+":"+ref); err != nil {
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

// Workspace returns the worktree named name when the clone has one, after
// forgetting worktrees whose directories are gone.
func (g *Git) Workspace(ctx context.Context, name string) (Worktree, bool, error) {
	if err := g.Prune(ctx); err != nil {
		return Worktree{}, false, err
	}
	out, err := g.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return Worktree{}, false, err
	}
	// Git lists the directory it resolved when the worktree was added.
	path := g.path(name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	for _, block := range strings.Split(out, "\n\n") {
		w := Worktree{Path: path, git: filepath.Join(g.Clone, ".git")}
		listed := ""
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				listed = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch refs/heads/"):
				w.Branch = strings.TrimPrefix(line, "branch refs/heads/")
			}
		}
		if listed == path || listed == resolved {
			return w, true, nil
		}
	}
	return Worktree{}, false, nil
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

// Prune forgets the clone's worktrees whose directories are gone.
func (g *Git) Prune(ctx context.Context) error {
	_, err := g.run(ctx, "worktree", "prune")
	return err
}

func (g *Git) path(name string) string { return filepath.Join(g.Directory, name) }

// run runs Git in the clone with hooks disabled and no prompt. The owner's
// Git configuration and agent stay in reach, because fetching the owner's
// remotes may need their credentials.
func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Clone, "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Env = []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	for _, name := range []string{"PATH", "HOME", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
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
