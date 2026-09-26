// Package workspace creates and inspects the service's workspaces on a
// project's clone behind the Provider interface, which also satisfies core's
// workspace provider interface. Git implements it with worktrees and Jujutsu
// with Jujutsu workspaces on the clone's Git store; the service alone runs
// them, and no agent holds them.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/vcs"
)

// Git is the worktree provider of one clone. Every workspace is a worktree of
// Clone under Directory, named by the request, on the branch the request
// names.
type Git struct {
	Clone     string
	Directory string
}

// Worktree is one workspace. Git's is a linked worktree whose repository
// metadata is the clone's; Jujutsu's is a Jujutsu workspace whose metadata is
// its repository's and the clone's.
type Worktree struct {
	Path   string
	Branch string
	mounts []string
}

func (w Worktree) Directory() string { return w.Path }
func (w Worktree) VCS() *vcs.Access  { return &vcs.Access{Mounts: w.mounts} }

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

// RemoteBranch asks a remote which commit one of its branches points at, and
// whether the branch exists. Nothing is fetched and no ref of the clone moves.
func (g *Git) RemoteBranch(ctx context.Context, remote, branch string) (string, bool, error) {
	env, err := g.sshEnvironment(ctx)
	if err != nil {
		return "", false, err
	}
	out, err := g.runEnv(ctx, env, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		commit, ref, ok := strings.Cut(line, "\t")
		if ok && ref == "refs/heads/"+branch {
			return commit, true, nil
		}
	}
	return "", false, nil
}

// Push points a remote's branch at commit, only while the remote branch is at
// expected, or absent when expected is empty; otherwise the remote refuses
// and nothing moves. SSH runs in batch mode as it does for Fetch.
func (g *Git) Push(ctx context.Context, remote, commit, branch, expected string) error {
	env, err := g.sshEnvironment(ctx)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	_, err = g.runEnv(ctx, env, "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+expected, remote, commit+":"+ref)
	return err
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

// Diff reads two recorded commits directly, independently of moving branches.
func (g *Git) Diff(ctx context.Context, base, candidate string) (string, error) {
	for _, revision := range []string{base, candidate} {
		if len(revision) != 40 {
			return "", fmt.Errorf("invalid recorded commit %q", revision)
		}
		for _, ch := range revision {
			if ch < '0' || ch > '9' && ch < 'a' || ch > 'f' {
				return "", fmt.Errorf("invalid recorded commit %q", revision)
			}
		}
		if _, err := g.run(ctx, "cat-file", "-e", revision+"^{commit}"); err != nil {
			return "", fmt.Errorf("recorded commit %s is unavailable: %w", revision, err)
		}
	}
	ancestor, err := g.Ancestor(ctx, base, candidate)
	if err != nil {
		return "", err
	}
	if !ancestor {
		return "", fmt.Errorf("recorded candidate %s does not descend from base %s", candidate, base)
	}
	return g.runInRaw(ctx, g.Clone, nil, "diff", "--no-ext-diff", "--binary", "--no-renames", base, candidate, "--")
}

// ChangedPaths lists the paths changed between the same recorded commits used
// for Diff, including both sides of a rename as separate changes.
func (g *Git) ChangedPaths(ctx context.Context, base, candidate string) ([]string, error) {
	if _, err := g.Diff(ctx, base, candidate); err != nil {
		return nil, err
	}
	out, err := g.runInRaw(ctx, g.Clone, nil, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base, candidate, "--")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
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
	if _, err := g.runIn(ctx, w.Path, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.excludesFile", "GIT_CONFIG_VALUE_0=" + os.DevNull}, "add", "--all"); err != nil {
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

// Squash returns one commit holding the tree of candidate whose only parent is
// base, with message, authored and committed by Osmia at the given time.
// Candidate must descend from base. The same arguments make the same commit,
// and no branch moves.
func (g *Git) Squash(ctx context.Context, base, candidate, message string, at time.Time) (string, error) {
	descends, err := g.Ancestor(ctx, base, candidate)
	if err != nil {
		return "", err
	}
	if !descends {
		return "", fmt.Errorf("candidate %s does not descend from %s", candidate, base)
	}
	tree, err := g.run(ctx, "rev-parse", "--verify", candidate+"^{tree}")
	if err != nil {
		return "", err
	}
	date := fmt.Sprintf("@%d +0000", at.Unix())
	env := append(slices.Clone(identity), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	return g.runEnv(ctx, env, "commit-tree", "--no-gpg-sign", "-p", base, "-m", message, tree)
}

// Rebase returns one commit whose only parent is onto and whose tree is the
// three-way merge of onto and head from their merge base, with message,
// authored and committed by Osmia at the given time, and the paths the merge
// left conflicted, sorted. A conflicted path holds the merge's conflict
// markers, or the side that kept it when the other deleted it. The same
// arguments make the same commit, and no branch moves.
func (g *Git) Rebase(ctx context.Context, onto, head, message string, at time.Time) (string, []string, error) {
	return g.RebaseFrom(ctx, "", onto, head, message, at)
}

// RebaseFrom rebases head onto onto using base as the three-way merge base.
// An empty base uses Git's ordinary merge base. For an explicit base, a
// temporary commit with onto's tree and base as parent gives merge-tree the
// requested ancestry without moving a branch.
func (g *Git) RebaseFrom(ctx context.Context, base, onto, head, message string, at time.Time) (string, []string, error) {
	date := fmt.Sprintf("@%d +0000", at.Unix())
	env := append(slices.Clone(identity), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	mergeOnto := onto
	if base != "" {
		tree, err := g.run(ctx, "rev-parse", "--verify", onto+"^{tree}")
		if err != nil {
			return "", nil, err
		}
		mergeOnto, err = g.runEnv(ctx, env, "commit-tree", "--no-gpg-sign", "-p", base, "-m", "Temporary rebase base for "+onto, tree)
		if err != nil {
			return "", nil, err
		}
	}
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", mergeOnto, head}
	out, err := g.runInRaw(ctx, g.Clone, nil, args...)
	if err != nil && !exitCode(err, 1) {
		return "", nil, err
	}
	fields := strings.Split(out, "\x00")
	tree := fields[0]
	if tree == "" {
		return "", nil, fmt.Errorf("git merge-tree of %s and %s wrote no tree", onto, head)
	}
	var conflicts []string
	for _, path := range fields[1:] {
		if path != "" && !slices.Contains(conflicts, path) {
			conflicts = append(conflicts, path)
		}
	}
	if err != nil && len(conflicts) == 0 {
		return "", nil, err
	}
	slices.Sort(conflicts)
	commit, err := g.runEnv(ctx, env, "commit-tree", "--no-gpg-sign", "-p", onto, "-m", message, tree)
	if err != nil {
		return "", nil, err
	}
	return commit, conflicts, nil
}

// Move points the worktree's branch at commit to, from commit from, and makes
// its index and files those of to. A worktree already at to has its index
// and files made those of to again, which completes a move interrupted
// between the two; one at any other commit is refused. Files the repository
// ignores stay unless to tracks them.
func (g *Git) Move(ctx context.Context, w Worktree, from, to string) error {
	head, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if head != to {
		if head != from {
			return fmt.Errorf("workspace %s is at %s, not %s", w.Path, head, from)
		}
		if _, err := g.runIn(ctx, w.Path, identity, "update-ref", "-m", "osmia: rebase", "HEAD", to, from); err != nil {
			return err
		}
	}
	_, err = g.runIn(ctx, w.Path, identity, "reset", "--hard", "--quiet", to)
	return err
}

// Change returns the change ID the workspace was created with, which a
// backend with change IDs gives every commit Carry makes of its work. Git
// commits carry no change ID, so Git returns "".
func (g *Git) Change(context.Context, Worktree) (string, error) { return "", nil }

// Record keeps the worktree's files, untracked files included, in a revision
// that moves no branch, and returns it for Changed to compare the worktree
// with later. Git keeps no such revision: it records nothing and returns "".
func (g *Git) Record(context.Context, Worktree) (string, error) { return "", nil }

// Changed records the worktree's files as Record does and returns the paths,
// sorted, whose files differ between the revision since, one Record returned,
// and the worktree. Git's Record returns no revision, so Git returns no paths.
func (g *Git) Changed(context.Context, Worktree, string) ([]string, error) { return nil, nil }

// ChangeOf returns the change ID commit carries, or "" when it carries none,
// which no Git commit does.
func (g *Git) ChangeOf(context.Context, string) (string, error) { return "", nil }

// Carry returns a commit holding what commit holds that carries change ID
// change. Git has no change IDs and returns commit itself.
func (g *Git) Carry(_ context.Context, commit, _ string) (string, error) { return commit, nil }

// Checkpoint durably records the backend's state before one attempt of a
// multi-step operation, named by operation, changes anything, so that
// Recover can restore it should the attempt be cut short; Settle drops the
// record once the attempt is over. Branches are not part of that state: a
// branch the attempt moved stays where it is, for the retry to reconcile.
// Git keeps no state of its own beside the clone's and records nothing.
func (g *Git) Checkpoint(context.Context, string, time.Time) error { return nil }

// Settle drops what Checkpoint recorded for the operation. Git recorded
// nothing.
func (g *Git) Settle(context.Context, string) error { return nil }

// Recover restores the state every checkpoint still recorded names, drops
// those checkpoints and returns the operations they were taken for. Git
// records none and returns none.
func (g *Git) Recover(context.Context) ([]string, error) { return nil, nil }

// StoredConflicts returns the paths commit holds as stored conflicts, which
// no Git commit does: a path Git's merge left conflicted holds its conflict
// markers as file content, which Markers finds.
func (g *Git) StoredConflicts(context.Context, string) ([]string, error) { return nil, nil }

// Markers returns the paths, of those given, whose file in commit still
// holds a conflict marker line: one that starts with seven < or seven >
// followed by a space or the line's end. A path commit does not hold as a
// file has none.
func (g *Git) Markers(ctx context.Context, commit string, paths []string) ([]string, error) {
	out, err := g.runInRaw(ctx, g.Clone, nil, "ls-tree", "-r", "-z", commit+"^{tree}")
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for _, entry := range strings.Split(out, "\x00") {
		info, path, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(info); ok && len(fields) == 3 && fields[1] == "blob" {
			blobs[path] = fields[2]
		}
	}
	var marked []string
	for _, path := range paths {
		blob, ok := blobs[path]
		if !ok {
			continue
		}
		content, err := g.runInRaw(ctx, g.Clone, nil, "cat-file", "blob", blob)
		if err != nil {
			return nil, err
		}
		if conflicted(content) {
			marked = append(marked, path)
		}
	}
	return marked, nil
}

// conflicted reports whether a file holds a conflict marker line.
func conflicted(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		for _, marker := range []string{"<<<<<<<", ">>>>>>>"} {
			if rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), marker); ok && (rest == "" || rest[0] == ' ') {
				return true
			}
		}
	}
	return false
}

// Advance fast-forwards the worktree's branch, with its index and files, from
// commit from to commit to, which must descend from it. A worktree already at
// to is left as it is; one at any other commit is refused.
func (g *Git) Advance(ctx context.Context, w Worktree, from, to string) error {
	head, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if head == to {
		return nil
	}
	if head != from {
		return fmt.Errorf("workspace %s is at %s, not %s", w.Path, head, from)
	}
	_, err = g.runIn(ctx, w.Path, identity, "merge", "--ff-only", "--quiet", to)
	return err
}

// replayName names the temporary worktree Replay runs in, under Directory.
// A leading dot keeps it apart from every workspace name.
const replayName = ".replay"

// Replay returns the commit that holds each commit head has and onto does
// not, applied on top of onto in order, each keeping its message and author
// and committed by Osmia at the given time, with the paths that conflicted,
// sorted, when that cannot be done. A commit whose change onto already holds
// is dropped, and a head that already descends from onto is returned as it
// is. The replay runs in a temporary detached worktree under Directory, which
// it removes, so no branch moves; the same arguments make the same commit.
func (g *Git) Replay(ctx context.Context, head, onto string, at time.Time) (string, []string, error) {
	descends, err := g.Ancestor(ctx, onto, head)
	if err != nil || descends {
		return head, nil, err
	}
	dir := g.path(replayName)
	if err := g.dropReplay(ctx, dir); err != nil {
		return "", nil, err
	}
	defer g.dropReplay(context.WithoutCancel(ctx), dir)
	if err := os.MkdirAll(g.Directory, 0700); err != nil {
		return "", nil, err
	}
	if _, err := g.run(ctx, "worktree", "add", "--quiet", "--detach", dir, head); err != nil {
		return "", nil, err
	}
	if _, err := g.runIn(ctx, dir, replayEnvironment(at), "rebase", "--quiet", "--no-autostash", onto); err != nil {
		conflicts, unmergedErr := g.unmerged(ctx, dir)
		if unmergedErr != nil || len(conflicts) == 0 {
			return "", nil, errors.Join(err, unmergedErr)
		}
		return "", conflicts, nil
	}
	commit, err := g.runIn(ctx, dir, nil, "rev-parse", "--verify", "HEAD^{commit}")
	return commit, nil, err
}

// replayEnvironment is the environment a replay runs Git in: commits are
// committed by Osmia at the given time, no editor opens and the owner's
// rebase settings that would change what is replayed are off.
func replayEnvironment(at time.Time) []string {
	date := fmt.Sprintf("@%d +0000", at.Unix())
	env := append(slices.Clone(identity), "GIT_COMMITTER_DATE="+date, "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	settings := []string{"rebase.autoStash=false", "rebase.autoSquash=false", "rebase.updateRefs=false", "rebase.rebaseMerges=false", "commit.gpgSign=false"}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(settings)))
	for i, setting := range settings {
		key, value, _ := strings.Cut(setting, "=")
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, value))
	}
	return env
}

// unmerged returns the paths the index of the worktree at dir holds
// unmerged, sorted.
func (g *Git) unmerged(ctx context.Context, dir string) ([]string, error) {
	out, err := g.runInRaw(ctx, dir, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

// ReplayIn replays the worktree's branch onto onto in the worktree itself,
// as Replay does, and returns the commit its branch then points at. A
// replay that conflicts stops at the commit it cannot apply, with the
// conflicted paths, sorted, holding conflict markers in the worktree, and
// returns those paths; ContinueReplay goes on once they are resolved. A
// worktree that already descends from onto is left as it is.
func (g *Git) ReplayIn(ctx context.Context, w Worktree, onto string, at time.Time) (string, []string, error) {
	return g.replayStep(ctx, w, at, "rebase", "--quiet", "--no-autostash", onto)
}

// ContinueReplay stages every file of the worktree, as Snapshot does, and
// goes on with the replay ReplayIn stopped, as it stopped: to the next
// commit that conflicts, whose paths it returns, or to the end, whose commit
// it returns. A replay step whose resolution leaves its commit's change
// empty drops the commit.
func (g *Git) ContinueReplay(ctx context.Context, w Worktree, at time.Time) (string, []string, error) {
	if _, err := g.runIn(ctx, w.Path, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.excludesFile", "GIT_CONFIG_VALUE_0=" + os.DevNull}, "add", "--all"); err != nil {
		return "", nil, err
	}
	return g.replayStep(ctx, w, at, "rebase", "--continue")
}

// replayStep runs one rebase command in the worktree and reports where it
// left the replay.
func (g *Git) replayStep(ctx context.Context, w Worktree, at time.Time, args ...string) (string, []string, error) {
	if _, err := g.runIn(ctx, w.Path, replayEnvironment(at), args...); err != nil {
		conflicts, unmergedErr := g.unmerged(ctx, w.Path)
		if unmergedErr != nil || len(conflicts) == 0 {
			return "", nil, errors.Join(err, unmergedErr)
		}
		return "", conflicts, nil
	}
	commit, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	return commit, nil, err
}

// Replaying returns the commit the replay in the worktree stopped at, with
// the paths its index holds unmerged, and whether a replay is in progress
// there. A replay in progress that stopped at no commit returns "".
func (g *Git) Replaying(ctx context.Context, w Worktree) (string, []string, bool, error) {
	dir, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")
	if err != nil {
		return "", nil, false, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return "", nil, false, nil
	} else if err != nil {
		return "", nil, false, err
	}
	stop, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", "--quiet", "REBASE_HEAD^{commit}")
	if err != nil && !exitCode(err, 1) {
		return "", nil, false, err
	}
	conflicts, err := g.unmerged(ctx, w.Path)
	return stop, conflicts, true, err
}

// MarkedFiles returns the paths, of those given, whose file in the worktree
// holds a conflict marker line, as Markers finds them in a commit. A path
// the worktree does not hold as a regular file has none.
func (g *Git) MarkedFiles(w Worktree, paths []string) ([]string, error) {
	return markedFiles(w.Path, paths)
}

// markedFiles returns the paths, of those given, whose regular file in the
// directory dir holds a conflict marker line.
func markedFiles(dir string, paths []string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var marked []string
	for _, path := range paths {
		info, err := root.Lstat(filepath.FromSlash(path))
		if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
			continue
		}
		if err != nil {
			return nil, err
		}
		content, err := root.ReadFile(filepath.FromSlash(path))
		if err != nil {
			return nil, err
		}
		if conflicted(string(content)) {
			marked = append(marked, path)
		}
	}
	return marked, nil
}

// dropReplay removes Replay's temporary worktree at dir, and what a stop left
// of it.
func (g *Git) dropReplay(ctx context.Context, dir string) error {
	entries, err := g.list(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if g.own(entry, replayName) {
			if err := g.forget(ctx, entry); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(dir)
}

// Export writes the tracked regular files of a commit's tree, with their
// executable bits, into the directory dir, which must not hold them yet.
// Symbolic links and submodules are left out.
func (g *Git) Export(ctx context.Context, commit, dir string) error {
	index, err := os.CreateTemp("", "osmia-export-*.index")
	if err != nil {
		return err
	}
	name := index.Name()
	defer os.Remove(name)
	if err := errors.Join(index.Close(), os.Remove(name)); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + name}
	if _, err := g.runEnv(ctx, env, "read-tree", commit+"^{tree}"); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if _, err := g.runEnv(ctx, append(env, "GIT_WORK_TREE="+dir), "checkout-index", "--all", "--force"); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Type().IsRegular() {
			return err
		}
		return os.Remove(path)
	})
}

// Commit is what a commit records.
type Commit struct {
	Tree    string
	Parents []string
	Message string
}

// Commit reads the tree, parents and message of a commit.
func (g *Git) Commit(ctx context.Context, revision string) (Commit, error) {
	out, err := g.runInRaw(ctx, g.Clone, nil, "show", "-s", "--format=%T%n%P%n%B", revision+"^{commit}", "--")
	if err != nil {
		return Commit{}, err
	}
	tree, rest, _ := strings.Cut(out, "\n")
	parents, message, _ := strings.Cut(rest, "\n")
	return Commit{Tree: tree, Parents: strings.Fields(parents), Message: strings.TrimRight(message, "\n")}, nil
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
// whose directory is gone is forgotten first, so it can be made again. A
// worktree a replay in progress holds detached is on the branch the replay
// returns to.
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
		branch := entry.branch
		if branch == "" {
			if branch, err = g.replayedBranch(ctx, g.path(name)); err != nil {
				return Worktree{}, false, err
			}
		}
		return Worktree{Path: g.path(name), Branch: branch, mounts: []string{filepath.Join(g.Clone, ".git")}}, true, nil
	}
	return Worktree{}, false, nil
}

// replayedBranch returns the branch the replay in progress in the worktree
// at dir returns to, or "" when no replay of a branch is in progress there.
func (g *Git) replayedBranch(ctx context.Context, dir string) (string, error) {
	path, err := g.runIn(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge/head-name")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	branch, _ := strings.CutPrefix(strings.TrimSpace(string(data)), "refs/heads/")
	if branch == "detached HEAD" {
		return "", nil
	}
	return branch, nil
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
	return Worktree{Path: path, Branch: req.Branch, mounts: []string{filepath.Join(g.Clone, ".git")}}, nil
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

// runIn runs Git the way run does, in dir rather than the clone. A command
// that fails returns what it wrote to standard output with its error.
func (g *Git) runIn(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	out, err := g.runInRaw(ctx, dir, extra, args...)
	return strings.TrimSpace(out), err
}

func (g *Git) runInRaw(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	return g.runInput(ctx, dir, extra, nil, args...)
}

// runInput runs Git the way runInRaw does, with input as its standard input.
func (g *Git) runInput(ctx context.Context, dir string, extra []string, input io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Stdin = input
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
		return stdout.String(), &Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), nil
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
