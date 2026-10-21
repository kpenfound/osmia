package intakefixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func AwaitFurtherPasses(t *testing.T, passed *atomic.Int64, n int64) {
	t.Helper()
	target := passed.Load() + n
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for passed.Load() < target {
		if time.Now().After(deadline) {
			t.Fatalf("reconciliation passes: %d, want at least %d", passed.Load(), target)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func AwaitCheck[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatal("check execution did not reach the expected boundary")
		var zero T
		return zero
	}
}

func Settled(t *testing.T, count func() int) {
	t.Helper()
	window := 50 * time.Millisecond
	last, stableSince := count(), time.Now()
	Soon(t, "activity to settle", func() bool {
		if n := count(); n != last {
			last, stableSince = n, time.Now()
		}
		return time.Since(stableSince) >= window
	})
}

func CheckNoRetry(t *testing.T, op trace.OperationRecord, outcome string) {
	t.Helper()
	if !op.Acknowledged || op.Result == nil || op.Result.Outcome != outcome || !op.RetryAt.IsZero() || slices.ContainsFunc(op.History, func(a trace.OperationAction) bool { return a.Kind == "retry" }) {
		t.Fatalf("turn operation: %+v", op)
	}
}

func CheckPauseStop(t *testing.T, q trace.QueuedTurn, scope, reason string) {
	t.Helper()
	r := q.Response
	want := trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: scope, Source: runtime.PauseOwner, Reason: reason}
	if r == nil || q.CompletedAt.IsZero() || q.Status() != "interrupted" || r.Stop == nil || *r.Stop != want || r.Failure != "" || r.FailureClass != "" || r.Classification != nil {
		t.Fatalf("turn %s was not stopped by the %s pause: %+v", q.Request.TurnID, scope, q)
	}
}

func AssertCheckGateReviewInstructions(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{"Test passing is decided by the service-owned check gate", "When the gate passed", "whole-suite, scoped or named", "satisfied as to passing", "evidence that required tests ran and passed", "Never raise a finding by itself", "only a smart-check selection ran", "cannot see that a named test was selected", "cannot run tests yourself", "tests the unit was asked to add exist and exercise the stated behaviour"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review instructions lack %q: %s", want, prompt)
		}
	}
}

func ReloadFails(t *testing.T, reload func(context.Context) (api.ReloadResponse, error), path, field string) {
	t.Helper()
	_, err := reload(context.Background())
	var failure *api.APIError
	if !errors.As(err, &failure) || failure.Code != api.Validation || !strings.Contains(failure.Message, path+": "+field+": ") {
		t.Fatalf("reload error: %v, want validation naming %s and %s", err, path, field)
	}
}

func TurnIDs(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent string) []string {
	t.Helper()
	th, err := repository.Thread(stream, agent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	Must(t, err)
	var ids []string
	for _, q := range th.Turns {
		ids = append(ids, q.Request.TurnID)
	}
	return ids
}

func LandingByUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, document func(string) string) map[string]trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	out := map[string]trace.Document{}
	for _, d := range docs {
		if d.Unit != "" && d.ID == document(d.Unit) {
			out[d.Unit] = d
		}
	}
	return out
}

func UnitRebases(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, document func(string) string) []api.UnitRebase {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	var out []api.UnitRebase
	for _, d := range docs {
		if d.ID == document(unit) {
			var r api.UnitRebase
			Must(t, json.Unmarshal([]byte(d.Content), &r))
			out = append(out, r)
		}
	}
	return out
}

func CurrentReport[R any](t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, document func(string) string) R {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	var report R
	for _, d := range docs {
		if d.ID == document(unit) {
			Must(t, json.Unmarshal([]byte(d.Content), &report))
		}
	}
	return report
}

func FactoryPauseOf(t *testing.T, runtimeView func(context.Context) (api.RuntimeResponse, error)) (runtime.Pause, bool) {
	t.Helper()
	rt, err := runtimeView(context.Background())
	Must(t, err)
	for _, p := range rt.Effective.Pauses {
		if p.Target == (runtime.Target{Scope: "factory"}) {
			return p, true
		}
	}
	return runtime.Pause{}, false
}

func MoveUnitNow(t *testing.T, now time.Time, actor trace.Actor, repository *trace.Repository, stream config.WorkstreamID, unit, to string) {
	t.Helper()
	subject := trace.UnitSubject(unit)
	state, err := repository.Workflow(stream, subject)
	Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("%s-moved-%s-%d", subject, to, state.Version), Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: now, Actor: actor, Cause: "test"}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "the test moves it"}})
	Must(t, err)
}

func CompleteDriftTurn(t *testing.T, turns *TurnScenario, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	Must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to run", agent)
	}
	token := "done-" + th.Turns[i].Request.TurnID
	directory := filepath.Join(turns.Root().String(), "threads", string(turns.Project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, turns.Now())
	Must(t, err)
	turns.CompleteClaimedTurn(t, repository, stream, agent, q, outcome)
	return q
}

func AwaitCompleted(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent, turn string) trace.QueuedTurn {
	t.Helper()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		th, err := repository.Thread(stream, agent)
		if err == nil {
			for _, q := range th.Turns {
				if q.Request.TurnID == turn && !q.CompletedAt.IsZero() {
					return q
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s of %s never completed: %+v %v", turn, agent, th.Turns, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func AwaitMasonSettled(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, turns int) {
	t.Helper()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		th, err := repository.Thread(stream, intake.MasonAgent(unit))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && len(th.Turns) >= turns && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mason of unit %s of %s never settled: %+v", unit, stream, th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func AwaitEventTurns(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, want ...string) {
	t.Helper()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		th, err := repository.ChiefOfStaffThread(stream)
		Must(t, err)
		found := 0
		for _, w := range want {
			for _, q := range th.Turns {
				if strings.HasPrefix(q.Request.TurnID, "events_") && !q.CompletedAt.IsZero() && q.Status() == "idle" && strings.Contains(q.Request.Prompt, w) {
					found++
					break
				}
			}
		}
		if found == len(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the chief of staff of %s never received event turns with %q: %v turns", stream, want, len(th.Turns))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func SettleOperation(t *testing.T, now time.Time, actor trace.Actor, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	ctx := context.Background()
	ops, err := repository.Operations(stream)
	Must(t, err)
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == op.ID })
	if i < 0 {
		t.Fatalf("operation %s is not recorded", op.ID)
	}
	var result coreadapter.OperationResult
	Must(t, repository.WithOperation(ctx, stream, ops[i].EventID, actor, func() time.Time { return now }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		seen, err := r.Inspect(ctx, op)
		if err != nil {
			return err
		}
		observe := a.Action("observe", now)
		observe.Observation = &seen
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if seen.State == coreadapter.EffectCompleted {
			result = *seen.Result
		} else {
			if err := a.Record(ctx, a.Action("effect", now)); err != nil {
				return err
			}
			if result, err = r.Apply(ctx, op); err != nil {
				return err
			}
		}
		done := a.Action("result", now)
		done.Result = &result
		return a.Record(ctx, done)
	}))
	return result
}

func DriftRecords[R any](t *testing.T, repository *trace.Repository, stream config.WorkstreamID, document string) []R {
	t.Helper()
	var out []R
	for _, d := range StreamDocuments(t, repository, stream, document) {
		var r R
		Must(t, json.Unmarshal([]byte(d.Content), &r))
		out = append(out, r)
	}
	return out
}

func DriftOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	Must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != action })
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	return ops
}

func RequestDrift(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string, request func(context.Context, func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error), due func(config.WorkstreamID) (string, error)) coreadapter.Operation {
	t.Helper()
	asked := len(DriftOperations(t, repository, stream, action))
	requested, err := request(context.Background(), due)
	Must(t, err)
	if !slices.Equal(requested, []config.WorkstreamID{stream}) {
		t.Fatalf("drift rebases asked for %v, want %s", requested, stream)
	}
	ops := DriftOperations(t, repository, stream, action)
	if len(ops) != asked+1 {
		t.Fatalf("drift operations %+v", ops)
	}
	return ops[asked].Operation
}

func AdvanceUpstream(t *testing.T, clone string, files map[string]string) string {
	t.Helper()
	home := filepath.Dir(clone)
	scratch := filepath.Join(home, "upstream-scratch")
	if _, err := os.Stat(scratch); err != nil {
		// --no-hardlinks: a local clone otherwise hardlinks the bare
		// repository's object files into scratch instead of copying them.
		fixturekit.DemoGit(t, home, "clone", "--quiet", "--no-hardlinks", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
	}
	for name, content := range files {
		Must(t, os.MkdirAll(filepath.Dir(filepath.Join(scratch, name)), 0700))
		Must(t, os.WriteFile(filepath.Join(scratch, name), []byte(content), 0600))
	}
	fixturekit.DemoGit(t, home, "-C", scratch, "add", "--all")
	fixturekit.DemoGit(t, home, "-C", scratch, "-c", "user.name=Upstream", "-c", "user.email=upstream@example.invalid", "commit", "-qm", "Upstream moves")
	fixturekit.DemoGit(t, home, "-C", scratch, "push", "--quiet", "origin", "HEAD:main")
	return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", scratch, "rev-parse", "HEAD"))
}

func OperationLog(t *testing.T, repository, home string) []string {
	t.Helper()
	cmd := exec.Command("jj", "--no-pager", "--color=never", "--repository", repository, "--ignore-working-copy", "operation", "log", "--no-graph", "--template", `id ++ " " ++ description ++ "\n"`)
	cmd.Env = []string{"JJ_CONFIG=" + os.DevNull, "PATH=" + os.Getenv("PATH"), "HOME=" + home}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj operation log: %v\n%s", err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func CheckpointEntry(t *testing.T, directory string, paths []string) string {
	t.Helper()
	if len(paths) != 1 {
		t.Fatalf("the %s workspaces hold checkpoints %v", directory, paths)
	}
	data, err := os.ReadFile(paths[0])
	Must(t, err)
	var c struct {
		Entry string `json:"entry"`
	}
	Must(t, json.Unmarshal(data, &c))
	if c.Entry == "" {
		t.Fatalf("the checkpoint records no entry: %s", data)
	}
	return c.Entry
}

func AwaitRefresh(t *testing.T, repository *trace.Repository, project config.ProjectID, action, agent string) trace.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for time.Now().Before(deadline) {
		ops, err := repository.Operations(intake.LibrarianWorkstream(project))
		Must(t, err)
		for _, op := range ops {
			if op.Operation.Action == action && op.Result != nil {
				return op
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	ops, _ := repository.Operations(intake.LibrarianWorkstream(project))
	thread, threadErr := repository.Thread(intake.LibrarianWorkstream(project), agent)
	t.Fatalf("refresh did not complete within %s: operations %+v; librarian thread %+v (error %v)", fixturekit.DemoTimeout, ops, thread, threadErr)
	return trace.OperationRecord{}
}
