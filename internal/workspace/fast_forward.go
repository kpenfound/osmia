package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/ghlog"
)

// FastForwardOutcome classifies what a fast-forward push to a remote branch
// reported.
type FastForwardOutcome string

const (
	// FastForwardPushed: the remote accepted the push, so its branch is at the
	// pushed commit.
	FastForwardPushed FastForwardOutcome = "pushed"
	// FastForwardBaseMoved: the remote rejected the push because its branch is
	// not at an ancestor of the commit: the push is not a fast-forward, or the
	// remote asks for a fetch first. Nothing moved.
	FastForwardBaseMoved FastForwardOutcome = "base-moved"
	// FastForwardRefused: the remote explicitly rejected the push, as a branch
	// protection, a pre-receive hook or a missing permission does. Nothing
	// moved, and pushing again is refused again.
	FastForwardRefused FastForwardOutcome = "refused"
	// FastForwardUnknown: the push ended with no verdict of the remote, as a
	// timeout, a dropped connection, a cancellation or an unreadable remote
	// does. The push may or may not have reached the remote.
	FastForwardUnknown FastForwardOutcome = "unknown"
)

// FastForwardResult is the classified outcome of a fast-forward push, with
// what Git and the remote said.
type FastForwardResult struct {
	Outcome FastForwardOutcome
	Output  string
}

// FastForward moves a remote's branch to commit with a plain push: never
// forced and never with a lease, so the remote accepts it only when the
// branch is at an ancestor of commit, and never creates a merge commit. The
// result classifies the push: pushed; base moved, when the remote rejects it
// as not a fast-forward; refused, when the remote explicitly rejects it; and
// unknown for everything else, which comes back with an error. Only an explicit
// verdict of the remote is classified as moved or refused. SSH runs in batch mode
// as it does for Fetch.
func (g *Git) FastForward(ctx context.Context, remote, commit, branch string) (FastForwardResult, error) {
	if err := networkUnlocked(ctx, g.Clone); err != nil {
		return FastForwardResult{Outcome: FastForwardUnknown, Output: err.Error()}, err
	}
	start := time.Now()
	result, err := g.fastForward(ctx, remote, commit, branch)
	ghlog.Log(ctx, start, ghlog.Entry{Action: "workspace.fast_forward", Access: ghlog.Write, Remote: remote, Branch: branch, Commits: nonEmpty(commit), Outcome: string(result.Outcome), Err: err})
	return result, err
}

func (g *Git) fastForward(ctx context.Context, remote, commit, branch string) (FastForwardResult, error) {
	env, err := g.sshEnvironment(ctx)
	if err != nil {
		return FastForwardResult{Outcome: FastForwardUnknown, Output: err.Error()}, err
	}
	ref := "refs/heads/" + branch
	_, err = g.runEnv(ctx, env, "push", "--no-verify", remote, commit+":"+ref)
	if err == nil {
		return FastForwardResult{Outcome: FastForwardPushed}, nil
	}
	var e *Error
	if !errors.As(err, &e) {
		return FastForwardResult{Outcome: FastForwardUnknown, Output: err.Error()}, err
	}
	switch outcome := ClassifyPushOutput(e.Stderr); outcome {
	case FastForwardBaseMoved, FastForwardRefused:
		return FastForwardResult{Outcome: outcome, Output: e.Stderr}, nil
	}
	return FastForwardResult{Outcome: FastForwardUnknown, Output: e.Stderr}, fmt.Errorf("push %s to %s of %s: %w", commit, branch, remote, err)
}

// ClassifyPushOutput classifies what a failed push printed. A remote that
// rejected the update explicitly (a hook or branch protection declined it, or
// the host denied permission) is refused. A push the remote rejected as not a
// fast-forward, or because the clone must fetch first, is base moved. Anything
// else, including a transport failure that holds no verdict of the remote, is
// unknown.
func ClassifyPushOutput(output string) FastForwardOutcome {
	text := strings.ToLower(output)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("[remote rejected]", "error: gh006", "protected branch", "pre-receive hook declined", "returned error: 403", "http 403", "error: 403"):
		return FastForwardRefused
	case has("permission to ") && has(" denied"):
		return FastForwardRefused
	case has("[rejected]") && has("non-fast-forward", "fetch first"):
		return FastForwardBaseMoved
	case has("updates were rejected because the tip of your current branch is behind", "updates were rejected because the remote contains work"):
		return FastForwardBaseMoved
	}
	return FastForwardUnknown
}
