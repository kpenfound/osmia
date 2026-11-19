package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// featureCommits makes n commits in the clone on top of from, without moving
// any branch, and returns the last. The clone holds from as it fetched it.
func (f fixture) featureCommits(t *testing.T, from string, n int) string {
	t.Helper()
	git(t, "-C", f.clone, "checkout", "--quiet", "--detach", from)
	for i := 0; i < n; i++ {
		name := "feature-" + string(rune('a'+i)) + ".txt"
		if err := os.WriteFile(filepath.Join(f.clone, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		git(t, "-C", f.clone, "add", name)
		git(t, "-C", f.clone, "commit", "--quiet", "-m", name)
	}
	return git(t, "-C", f.clone, "rev-parse", "HEAD")
}

func (f fixture) upstreamMain(t *testing.T) string {
	t.Helper()
	return git(t, "-C", f.upstream, "rev-parse", "refs/heads/main")
}

func TestFastForwardMovesTheBaseToTheCommitWithoutAMergeCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	tip := f.featureCommits(t, base, 3)
	result, err := f.provider.FastForward(ctx, "upstream", tip, "main")
	if err != nil || result.Outcome != FastForwardPushed {
		t.Fatalf("the fast-forward is %+v: %v", result, err)
	}
	if got := f.upstreamMain(t); got != tip {
		t.Fatalf("the base branch is at %s, want exactly %s", got, tip)
	}
	if merges := git(t, "-C", f.upstream, "rev-list", "--merges", base+".."+tip); merges != "" {
		t.Fatalf("the landing created merge commits: %s", merges)
	}
	if n := strings.Fields(git(t, "-C", f.upstream, "rev-list", base+".."+tip)); len(n) != 3 {
		t.Fatalf("the history between the old base and the new tip is %d commits, want the feature's 3", len(n))
	}
	// Landing the same commit again is a push of what is already there.
	if again, err := f.provider.FastForward(ctx, "upstream", tip, "main"); err != nil || again.Outcome != FastForwardPushed {
		t.Fatalf("landing what the base holds is %+v: %v", again, err)
	}
}

func TestFastForwardReportsAMovedBaseAndMovesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	tip := f.featureCommits(t, base, 1)
	moved := f.advance(t, "elsewhere.txt")
	for _, fetched := range []bool{false, true} {
		if fetched {
			if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
				t.Fatal(err)
			}
		}
		result, err := f.provider.FastForward(ctx, "upstream", tip, "main")
		if err != nil || result.Outcome != FastForwardBaseMoved {
			t.Fatalf("with the moved base fetched=%v the push is %+v: %v", fetched, result, err)
		}
		if got := f.upstreamMain(t); got != moved {
			t.Fatalf("a rejected push moved the base to %s, want %s", got, moved)
		}
	}
	// A commit the base already passed is not forced over it either.
	if result, err := f.provider.FastForward(ctx, "upstream", base, "main"); err != nil || result.Outcome != FastForwardBaseMoved {
		t.Fatalf("pushing an ancestor of the base is %+v: %v", result, err)
	}
	if got := f.upstreamMain(t); got != moved {
		t.Fatalf("the base was rewound to %s", got)
	}
}

func TestFastForwardReportsAnExplicitRefusalAsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	tip := f.featureCommits(t, base, 1)
	hook := filepath.Join(f.upstream, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'error: GH006: Protected branch update failed for refs/heads/main.' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := f.provider.FastForward(ctx, "upstream", tip, "main")
	if err != nil || result.Outcome != FastForwardRefused {
		t.Fatalf("a protected branch is %+v: %v", result, err)
	}
	if !strings.Contains(result.Output, "Protected branch") {
		t.Fatalf("the refusal does not carry the remote's words: %q", result.Output)
	}
	if got := f.upstreamMain(t); got != base {
		t.Fatalf("a refused push moved the base to %s", got)
	}
}

func TestFastForwardCallsAnythingWithoutAVerdictUnknown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	tip := f.featureCommits(t, base, 1)

	// The push goes where nothing answers: a dropped connection or a host that
	// cannot be read says nothing of the upstream.
	git(t, "-C", f.clone, "config", "remote.upstream.pushurl", filepath.Join(filepath.Dir(f.upstream), "gone.git"))
	result, err := f.provider.FastForward(ctx, "upstream", tip, "main")
	if err == nil || result.Outcome != FastForwardUnknown {
		t.Fatalf("an unreachable remote is %+v: %v", result, err)
	}
	git(t, "-C", f.clone, "config", "--unset", "remote.upstream.pushurl")
	if got := f.upstreamMain(t); got != base {
		t.Fatalf("the base moved to %s", got)
	}

	// A cancellation is no verdict either, and nothing was pushed by it.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if result, err := f.provider.FastForward(cancelled, "upstream", tip, "main"); err == nil || result.Outcome != FastForwardUnknown {
		t.Fatalf("a cancelled push is %+v: %v", result, err)
	}
	if got := f.upstreamMain(t); got != base {
		t.Fatalf("the base moved to %s", got)
	}
}

func TestClassifyPushOutputTakesOnlyTheRemotesExplicitVerdicts(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, output string
		want         FastForwardOutcome
	}{
		{"pre-receive hook", " ! [remote rejected] main -> main (pre-receive hook declined)", FastForwardRefused},
		{"branch protection", "remote: error: GH006: Protected branch update failed for refs/heads/main.\n ! [remote rejected] main -> main (protected branch hook declined)", FastForwardRefused},
		{"permission denied by the host", "remote: Permission to acme/widgets.git denied to osmia.\nfatal: unable to access 'https://github.com/acme/widgets.git/': The requested URL returned error: 403", FastForwardRefused},
		{"http 403", "fatal: unable to access 'https://example.invalid/r.git/': The requested URL returned error: 403", FastForwardRefused},
		{"not a fast-forward", " ! [rejected]        abc -> main (non-fast-forward)\nhint: Updates were rejected", FastForwardBaseMoved},
		{"fetch first", " ! [rejected]        abc -> main (fetch first)", FastForwardBaseMoved},
		{"behind", "hint: Updates were rejected because the tip of your current branch is behind", FastForwardBaseMoved},
		{"ssh credentials", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", FastForwardUnknown},
		{"no route", "fatal: unable to access 'https://github.com/acme/widgets.git/': Could not resolve host: github.com", FastForwardUnknown},
		{"timeout", "fatal: unable to access 'https://github.com/acme/widgets.git/': Operation timed out after 30001 milliseconds", FastForwardUnknown},
		{"dropped", "send-pack: unexpected disconnect while reading sideband packet\nfatal: the remote end hung up unexpectedly", FastForwardUnknown},
		{"nothing", "", FastForwardUnknown},
	} {
		if got := ClassifyPushOutput(c.output); got != c.want {
			t.Errorf("%s: classified %q as %s, want %s", c.name, c.output, got, c.want)
		}
	}
}
