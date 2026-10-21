package intakefixture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ArchiveRecord(t *testing.T, store *runtime.Store, stream config.WorkstreamID) (runtime.Archive, bool) {
	t.Helper()
	state, _ := store.Snapshot()
	for _, a := range state.Archived {
		if a.Workstream == stream {
			return a, true
		}
	}
	return runtime.Archive{}, false
}

func OnlyRun[R any](t *testing.T, repository *trace.Repository, stream config.WorkstreamID, read func(*trace.Repository, config.WorkstreamID) ([]R, error)) R {
	t.Helper()
	runs, err := read(repository, stream)
	Must(t, err)
	if len(runs) != 1 {
		t.Fatalf("check runs %+v, want one", runs)
	}
	return runs[0]
}

func ReportDone(outcome string) func(context.Context, agent.Request, *mcp.ClientSession) error {
	return func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := Done(ctx, tools, map[string]any{"outcome": outcome})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
}

func ApplyCheck(ctx context.Context, c coreadapter.Reconciler, op coreadapter.Operation) <-chan error {
	finished := make(chan error, 1)
	go func() {
		_, err := c.Apply(ctx, op)
		finished <- err
	}()
	return finished
}

func HeldCheck(ctx context.Context, links []string, entered chan []string, release chan fixturekit.CheckOutcome) (checks.CheckResult, error) {
	entered <- slices.Clone(links)
	select {
	case o := <-release:
		return o.Result, o.Err
	case <-ctx.Done():
		return checks.CheckResult{ExitCode: -1}, ctx.Err()
	}
}

func CleanupCheckpoint(ctx context.Context, step string, reached, released chan struct{}, once *sync.Once) error {
	if step != "archive-cleanup-ready" {
		return nil
	}
	once.Do(func() { close(reached) })
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func AwaitCleanup(t *testing.T, reached chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("live controller did not reach eligible archive cleanup")
	}
}

func MissingVerdictContest(t *testing.T, state func(*testing.T) trace.WorkflowState, pass func(context.Context, trace.WorkflowState) error, capture func(), readContest func() (trace.Transition, bool, error)) trace.Transition {
	t.Helper()
	ctx := context.Background()
	Must(t, pass(ctx, state(t)))
	capture()
	Must(t, pass(ctx, state(t)))
	if state := state(t); state.Value != intake.UnitContested {
		t.Fatalf("a review turn without a verdict left the unit %s", state.Value)
	}
	contest, contested, err := readContest()
	Must(t, err)
	if !contested || !strings.Contains(contest.Reason, "ended without recording a verdict") {
		t.Fatalf("contest %+v", contest)
	}
	return contest
}

func LandingReview(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession, record func(string, review.UnitReviewIdentity)) (*agent.Result, error) {
	identity, err := review.IdentityInPrompt(req.Prompt)
	if err != nil {
		return nil, err
	}
	record(unit, identity)
	body, err := CallTool(ctx, tools, "verdict", map[string]any{"decision": "satisfactory", "summary": "The candidate does the task and its acceptance holds", "findings": []api.ReviewFinding{}})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("verdict %s: %v", body, err)
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

func PrepareJujutsuLanding[B any](t *testing.T, repository *trace.Repository, stream config.WorkstreamID, d UnitSeedDriver[B], find func(context.Context, config.WorkstreamID, string) (workspace.Worktree, string, bool, error), filesByUnit map[string]map[string]string, upload, audit string, move func(map[string]string) string, pass func(context.Context) error, operations func(string) []trace.OperationRecord) (string, map[string]coreadapter.Operation) {
	t.Helper()
	ctx := context.Background()
	if backend, err := repository.Workspaces(stream); err != nil || backend != config.WorkspacesJujutsu {
		t.Fatalf("workstream %s is on %q: %v", stream, backend, err)
	}

	for unit, report := range map[string]string{"upload": upload, "audit": audit} {
		b, found, err := d.Read(stream)
		Must(t, err)
		if !found {
			t.Fatal("the workstream is not building")
		}
		if started, _, err := d.Start(ctx, b, unit); err != nil || !started {
			t.Fatalf("%s did not start: %v", unit, err)
		}
		d.Complete(t, repository, stream, unit, report)
	}

	for unit, files := range filesByUnit {
		w, _, found, err := find(ctx, stream, unit)
		Must(t, err)
		if !found {
			t.Fatalf("unit %s has no workspace", unit)
		}
		for name, content := range files {
			if content == "" {
				Must(t, os.Remove(filepath.Join(w.Path, name)))
				continue
			}
			Must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, name)), 0700))
			Must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
		}
	}
	landed := move(map[string]string{fixturekit.MasonWrote: "package trace\n\n// landed\n", "internal/trace/git.go": "package trace\n\n// landed git\n"})

	Must(t, pass(ctx))
	ops := map[string]coreadapter.Operation{}
	for _, unit := range []string{"resume", "upload", "audit"} {
		asked := operations(unit)
		if len(asked) != 1 {
			t.Fatalf("one pass asked to rebase unit %s %d times", unit, len(asked))
		}
		ops[unit] = asked[0].Operation
	}
	return landed, ops
}

func UnitOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, identify func(coreadapter.Operation) (string, error)) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	Must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool {
		found, err := identify(o.Operation)
		return err != nil || found != unit
	})
}

func Workspace(t *testing.T, provider workspace.Provider, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	w, found, err := provider.Workspace(context.Background(), string(stream))
	Must(t, err)
	if !found {
		t.Fatal("the workstream has no drift resolution workspace")
	}
	return w
}

func VerdictOutcome(t *testing.T, verdict any, status string) *coreadapter.Outcome {
	t.Helper()
	data, err := json.Marshal(verdict)
	Must(t, err)
	return &coreadapter.Outcome{Status: status, Report: string(data)}
}
