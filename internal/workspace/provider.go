package workspace

import (
	"context"
	"time"

	"github.com/kpenfound/busybees/core/vcs"
)

// Provider is the version control backend of one set of workspaces on a
// project's clone: everything the service asks of version control goes
// through it. A commit is the backend's immutable revision ID, and a branch
// is a named, movable pointer at one: a Git branch, or a Jujutsu bookmark. A
// Worktree is one checked-out workspace: its Path is the directory an agent's
// files come from and its Branch the branch the workspace works on.
//
// Every operation that makes a commit moves no branch unless its
// documentation says so, and is deterministic for its arguments but for a
// Jujutsu rebase that conflicts; conflicted paths come back sorted. The
// methods are documented on Git, and where Jujutsu differs, on Jujutsu.
type Provider interface {
	vcs.Provider

	// Workspace lookup and upkeep.
	Workspace(ctx context.Context, name string) (Worktree, bool, error)
	Prune(ctx context.Context) error

	// Branches and history.
	Branch(ctx context.Context, name string) (string, bool, error)
	Ancestor(ctx context.Context, commit, tip string) (bool, error)
	MergeBase(ctx context.Context, a, b string) (string, error)
	Commit(ctx context.Context, revision string) (Commit, error)
	Diff(ctx context.Context, base, candidate string) (string, error)
	ChangedPaths(ctx context.Context, base, candidate string) ([]string, error)
	Markers(ctx context.Context, commit string, paths []string) ([]string, error)
	StoredConflicts(ctx context.Context, commit string) ([]string, error)
	Export(ctx context.Context, commit, dir string) error

	// Making commits.
	Snapshot(ctx context.Context, w Worktree, base string) (string, error)
	Squash(ctx context.Context, base, candidate, message string, at time.Time) (string, error)
	Rebase(ctx context.Context, onto, head, message string, at time.Time) (string, []string, error)
	RebaseFrom(ctx context.Context, base, onto, head, message string, at time.Time) (string, []string, error)
	Replay(ctx context.Context, head, onto string, at time.Time) (string, []string, error)

	// Change identity.
	Change(ctx context.Context, w Worktree) (string, error)
	ChangeOf(ctx context.Context, commit string) (string, error)
	Carry(ctx context.Context, commit, change string) (string, error)

	// Moving a workspace, and replaying in it with conflicts to resolve.
	Move(ctx context.Context, w Worktree, from, to string) error
	Advance(ctx context.Context, w Worktree, from, to string) error
	ReplayIn(ctx context.Context, w Worktree, onto string, at time.Time) (string, []string, error)
	ContinueReplay(ctx context.Context, w Worktree, at time.Time) (string, []string, error)
	Replaying(ctx context.Context, w Worktree) (string, []string, bool, error)
	MarkedFiles(w Worktree, paths []string) ([]string, error)

	// Remotes.
	Remote(ctx context.Context, repository string) (string, error)
	Fetch(ctx context.Context, remote, branch string) (string, error)
	RemoteBranch(ctx context.Context, remote, branch string) (string, bool, error)
	Push(ctx context.Context, remote, commit, branch, expected string) error
}

var _ Provider = (*Git)(nil)
