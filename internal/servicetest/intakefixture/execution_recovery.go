package intakefixture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func StopAt(t *testing.T, step string, bind func(func(string) error)) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := false
	bind(func(name string) error {
		if name == step && !stopped {
			stopped = true
			cancel()
			return errors.New("the service stopped")
		}
		return nil
	})
	t.Cleanup(func() {
		cancel()
		bind(nil)
	})
	return ctx
}

func RestoredTo(t *testing.T, directory, entry string, changed bool, log, paths []string) {
	t.Helper()
	restored := slices.ContainsFunc(log, func(line string) bool { return strings.HasSuffix(line, " restore to operation "+entry) })
	if changed != restored {
		t.Fatalf("the %s workspaces were restored to %s: %t, want %t:\n%s", directory, entry, restored, changed, strings.Join(log, "\n"))
	}
	if len(paths) != 0 {
		t.Fatalf("checkpoints left after the restore: %v", paths)
	}
}

func OnCommit(t *testing.T, g workspace.Provider, w workspace.Worktree, commit string) {
	t.Helper()
	ctx := context.Background()
	current, err := g.Record(ctx, w)
	Must(t, err)
	c, err := g.Commit(ctx, current)
	Must(t, err)
	if !slices.Equal(c.Parents, []string{commit}) {
		t.Fatalf("workspace %s is on %v, not %s", w.Path, c.Parents, commit)
	}
	if changed, err := g.Changed(ctx, w, commit); err != nil || len(changed) != 0 {
		t.Fatalf("workspace %s differs from %s in %v: %v", w.Path, commit, changed, err)
	}
}

func ClaimOldestTurn(t *testing.T, turns *TurnScenario, repository *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	th, err := repository.Thread(stream, agent)
	Must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to claim", agent)
	}
	token := "claim-" + th.Turns[i].Request.TurnID
	directory := filepath.Join(turns.Root().String(), "threads", string(turns.Project), string(stream), agent, token)
	q, err := repository.ClaimTurn(context.Background(), stream, agent, token, directory, turns.Now())
	Must(t, err)
	return q
}

func AskTool(t *testing.T, list []coreadapter.Tool, name string) coreadapter.Tool {
	t.Helper()
	i := slices.IndexFunc(list, func(tool coreadapter.Tool) bool { return tool.Name == name })
	if i < 0 {
		var names []string
		for _, tool := range list {
			names = append(names, tool.Name)
		}
		t.Fatalf("no tool %s among %v", name, names)
	}
	return list[i]
}

func StartFeedChecks(t *testing.T, root string, now func() time.Time, action string, bind func(chan time.Time, func(config.WorkstreamID, coreadapter.Operation) bool), start, stop func(*testing.T)) func() {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	Must(t, err)
	_, err = file.WriteString("\n[listen]\nweb = \"127.0.0.1:0\"\n")
	Must(t, err)
	Must(t, file.Close())
	var ready atomic.Bool
	ticks := make(chan time.Time, 1)
	bind(ticks, func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return !ready.Load() || op.Action != action
	})
	start(t)
	t.Cleanup(func() { stop(t) })
	return func() { ready.Store(true); ticks <- now() }
}

// JujutsuRepository identifies the fixture project's workspace repository.
func JujutsuRepository(root config.Root, project config.ProjectID, directory string) string {
	return filepath.Join(root.String(), directory, string(project), ".jujutsu")
}

func Checkpoints(t *testing.T, repository string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repository, ".jj", "osmia-checkpoints", "*.json"))
	Must(t, err)
	return paths
}

func Interrupted(t *testing.T, directory string, paths, log []string) (string, bool) {
	t.Helper()
	if len(paths) > 0 {
		entry := CheckpointEntry(t, directory, paths)
		return entry, !strings.HasPrefix(log[0], entry+" ")
	}
	for _, line := range log {
		if _, entry, ok := strings.Cut(line, " restore to operation "); ok {
			return entry, true
		}
	}
	// Restoring the current entry records no new operation when the step
	// changed no jj state (for example, a Git-only squash or replay).
	if len(log) > 0 {
		entry, _, _ := strings.Cut(log[0], " ")
		return entry, false
	}
	t.Fatalf("the cancelled %s local step has no operation log", directory)
	return "", false
}
