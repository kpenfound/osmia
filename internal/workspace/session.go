package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// WorktreeDiff reads the worktree's uncommitted changes relative to its own
// commit, untracked files included and files the repository ignores left
// out, as a unified diff, without changing the worktree's index or files: it
// stages everything into a disposable temporary index first, the same way
// Export populates one, then diffs that index against HEAD.
func (g *Git) WorktreeDiff(ctx context.Context, w Worktree) (string, error) {
	if w.Path == "" {
		return "", errors.New("no workspace to diff")
	}
	index, err := os.CreateTemp("", "osmia-worktree-diff-*.index")
	if err != nil {
		return "", err
	}
	name := index.Name()
	if err := errors.Join(index.Close(), os.Remove(name)); err != nil {
		return "", err
	}
	defer os.Remove(name)
	env := []string{"GIT_INDEX_FILE=" + name, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.excludesFile", "GIT_CONFIG_VALUE_0=" + os.DevNull}
	if _, err := g.runIn(ctx, w.Path, env, "add", "--all"); err != nil {
		return "", err
	}
	return g.runInRaw(ctx, w.Path, env, "diff", "--no-ext-diff", "--binary", "--no-renames", "--cached", "HEAD", "--")
}

// CommitChanges commits every change the worktree holds, untracked files
// included and files the repository ignores left out, onto its branch with
// message, authored and committed by Osmia at the given time, and reports
// the new commit and whether the worktree held a change to commit. A
// worktree whose tree already matches its own commit holds nothing to
// commit, and its unchanged commit is returned with ok false, the same test
// Snapshot uses for an unchanged worktree.
func (g *Git) CommitChanges(ctx context.Context, w Worktree, message string, at time.Time) (string, bool, error) {
	if w.Path == "" {
		return "", false, errors.New("no workspace to commit")
	}
	head, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", false, err
	}
	if _, err := g.runIn(ctx, w.Path, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.excludesFile", "GIT_CONFIG_VALUE_0=" + os.DevNull}, "add", "--all"); err != nil {
		return "", false, err
	}
	tree, err := g.runIn(ctx, w.Path, nil, "write-tree")
	if err != nil {
		return "", false, err
	}
	current, err := g.runIn(ctx, w.Path, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return "", false, err
	}
	if tree == current {
		return head, false, nil
	}
	date := fmt.Sprintf("@%d +0000", at.Unix())
	env := append(slices.Clone(identity), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	commit, err := g.runIn(ctx, w.Path, env, "commit-tree", "--no-gpg-sign", "-p", head, "-m", message, tree)
	if err != nil {
		return "", false, err
	}
	if _, err := g.runIn(ctx, w.Path, identity, "update-ref", "-m", "osmia: commit", "HEAD", commit, head); err != nil {
		return "", false, err
	}
	return commit, true, nil
}

// Log reads the commits strictly after base on head, oldest first, each
// with its message. head equal to base reports no commits. Every session
// commit CommitChanges makes has exactly one parent, the one before it, so
// the history between base and head is always linear.
func (g *Git) Log(ctx context.Context, base, head string) ([]DeliveryMessage, error) {
	if base == head {
		return nil, nil
	}
	out, err := g.run(ctx, "rev-list", "--reverse", base+".."+head)
	if err != nil {
		return nil, err
	}
	var messages []DeliveryMessage
	parent := base
	for _, id := range strings.Fields(out) {
		c, err := g.Commit(ctx, id)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(c.Parents, []string{parent}) {
			return nil, errors.New("session commits must form a linear history on the base")
		}
		messages = append(messages, DeliveryMessage{Commit: id, Message: c.Message})
		parent = id
	}
	return messages, nil
}
