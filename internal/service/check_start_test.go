package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// heldChecks lets a test inspect the durable trace while execution is held
// at the check runner boundary, then complete or interrupt that execution.
type heldChecks struct {
	links   []string
	entered chan []string
	release chan checkOutcome
}

func newHeldChecks() *heldChecks {
	return &heldChecks{entered: make(chan []string, 1), release: make(chan checkOutcome, 1)}
}

func (c *heldChecks) List(context.Context, string) ([]string, error) { return c.links, nil }

func (c *heldChecks) Check(ctx context.Context, _ string, links []string) (CheckResult, error) {
	c.entered <- slices.Clone(links)
	select {
	case o := <-c.release:
		return o.Result, o.Err
	case <-ctx.Done():
		return CheckResult{ExitCode: -1}, ctx.Err()
	}
}

func awaitCheck[T any](t *testing.T, ch <-chan T) T {
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

func applyCheck(ctx context.Context, c coreadapter.Reconciler, op coreadapter.Operation) <-chan error {
	finished := make(chan error, 1)
	go func() {
		_, err := c.Apply(ctx, op)
		finished <- err
	}()
	return finished
}

func TestUnitCheckSelectionIsReadableBeforeChecksFinish(t *testing.T) {
	t.Parallel()
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			t.Parallel()
			checks := newHeldChecks()
			// Narrow many test links to one package, so judged and listed
			// counts are observably different.
			for i := range checkselect.MaxQuestions + 1 {
				checks.links = append(checks.links, fmt.Sprintf("dag+check://go/packages/tests/test?go-package=internal/trace&go-test=Test%d", i))
			}
			f, stream, repo := newChecksFixture(t, "selection-start", checks)
			probability := 0.9
			if full {
				probability = 0.1
			}
			provider := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(probability)}}}}}
			boostJev(f.s, provider)
			c := &checkers{masons: newMasonController(f.s, repo)}
			must(t, c.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			must(t, err)
			i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == CheckAction })
			op := ops[i].Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := applyCheck(ctx, c, op)
			ran := awaitCheck(t, checks.entered)
			docs := streamDocuments(t, repo, stream, checkSelectionDocument("resume", 1))
			if len(docs) != 1 {
				t.Fatalf("selection records while running: %+v", docs)
			}
			var start UnitCheckSelection
			must(t, json.Unmarshal([]byte(docs[0].Content), &start))
			selection := start.Selection
			if selection.Listed == nil || *selection.Listed != len(checks.links) || selection.Candidates != 1 || selection.Judgment == "" || !slices.Equal(selection.Links, ran) {
				t.Fatalf("running selection %+v, runner links %v", selection, ran)
			}
			if full {
				if selection.Mode != selectionFull || selection.Reason != string(jev.ReasonDeclined) || selection.Detail == "" || len(ran) != 0 {
					t.Fatalf("full selection %+v", selection)
				}
			} else if selection.Mode != selectionSelected || len(ran) != 1 {
				t.Fatalf("selected selection %+v", selection)
			}
			if observed, err := c.Inspect(ctx, op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
				t.Fatalf("start completed the operation: %+v %v", observed, err)
			}
			state, err := repo.Workflow(stream, checksSubject("resume"))
			must(t, err)
			if state.Value != "requested-1" || unitState(t, repo, stream, "resume") != UnitChecking {
				t.Fatalf("start changed workflow: %+v", state)
			}
			checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			must(t, awaitCheck(t, finished))
			run := onlyRun(t, repo, stream)
			if run.Status != ChecksPassed || !reflect.DeepEqual(*run.Selection, selection) || !run.StartedAt.Equal(start.StartedAt) {
				t.Fatalf("finished run changed selection: %+v", run)
			}
		})
	}
}

func TestUnitCheckRestartReusesOneSelectionRecord(t *testing.T) {
	t.Parallel()
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			t.Parallel()
			checks := newHeldChecks()
			checks.links = []string{"dag+check://go/test"}
			f, stream, repo := newChecksFixture(t, "selection-restart", checks)
			p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.9)}}}}}
			if !full {
				boostJev(f.s, p)
			}
			c := &checkers{masons: newMasonController(f.s, repo)}
			must(t, c.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			must(t, err)
			i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == CheckAction })
			op := ops[i].Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := applyCheck(ctx, c, op)
			firstLinks := awaitCheck(t, checks.entered)
			before := streamDocuments(t, repo, stream, checkSelectionDocument("resume", 1))
			cancel()
			if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run: %v", err)
			}
			must(t, repo.Close())
			reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			t.Cleanup(func() { reopened.Close() })
			c = &checkers{masons: newMasonController(f.s, reopened)}
			if runs, err := checkRuns(reopened, stream); err != nil || len(runs) != 0 {
				t.Fatalf("interrupted run has a result: %+v %v", runs, err)
			}
			if got := streamDocuments(t, reopened, stream, checkSelectionDocument("resume", 1)); len(before) != 1 || !reflect.DeepEqual(before, got) {
				t.Fatalf("start lost on restart: %+v", got)
			}
			if observed, err := c.Inspect(context.Background(), op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
				t.Fatalf("interrupted run completed: %+v %v", observed, err)
			}
			// Change both listing and boost settings: the run's original
			// choice, even a disabled full run, must still be reused.
			checks.links = []string{"dag+check://different/test"}
			boostJev(f.s, &jevtest.Provider{})
			finished = applyCheck(context.Background(), c, op)
			if links := awaitCheck(t, checks.entered); !slices.Equal(firstLinks, links) {
				t.Fatalf("restart changed check links: %v, originally %v", links, firstLinks)
			}
			checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			must(t, awaitCheck(t, finished))
			if got := streamDocuments(t, reopened, stream, checkSelectionDocument("resume", 1)); !reflect.DeepEqual(before, got) {
				t.Fatalf("restart duplicated or rewrote selection: %+v", got)
			}
			var start UnitCheckSelection
			must(t, json.Unmarshal([]byte(before[0].Content), &start))
			if run := onlyRun(t, reopened, stream); !reflect.DeepEqual(start.Selection, *run.Selection) || !start.StartedAt.Equal(run.StartedAt) {
				t.Fatalf("restart result changed selection: %+v", run)
			}
			if n := len(p.Requests()); (!full && n != 1) || (full && n != 0) {
				t.Fatalf("Jev received %d requests", n)
			}
		})
	}
}

func TestUnitCheckRestartTerminalResultPreservesSelection(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"preparation failure", "runner unavailable", "superseded"} {
		t.Run(terminal, func(t *testing.T) {
			t.Parallel()
			checks := newHeldChecks()
			checks.links = []string{"dag+check://go/test"}
			f, stream, repo := newChecksFixture(t, "selection-terminal-restart", checks)
			probability := 0.9
			if terminal == "runner unavailable" {
				probability = 0.1
			}
			provider := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(probability)}}}}}
			boostJev(f.s, provider)
			c := &checkers{masons: newMasonController(f.s, repo)}
			must(t, c.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			must(t, err)
			i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == CheckAction })
			op := ops[i].Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := applyCheck(ctx, c, op)
			awaitCheck(t, checks.entered)
			before := streamDocuments(t, repo, stream, checkSelectionDocument("resume", 1))
			if len(before) != 1 {
				t.Fatalf("running selection records: %+v", before)
			}
			var start UnitCheckSelection
			must(t, json.Unmarshal([]byte(before[0].Content), &start))
			if start.Selection.Listed == nil || *start.Selection.Listed != len(checks.links) {
				t.Fatalf("running selection lost listed total: %+v", start.Selection)
			}
			cancel()
			if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run: %v", err)
			}
			must(t, repo.Close())
			reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			t.Cleanup(func() { reopened.Close() })
			c = &checkers{masons: newMasonController(f.s, reopened)}
			if observed, err := c.Inspect(context.Background(), op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
				t.Fatalf("interrupted run completed: %+v %v", observed, err)
			}
			wantStatus, wantError := ChecksIncomplete, ""
			switch terminal {
			case "preparation failure":
				// Prevent preparation of the next export directory.
				base := filepath.Join(f.s.cfg.Root.String(), "checks")
				must(t, os.RemoveAll(base))
				must(t, os.WriteFile(base, []byte("blocks export preparation"), 0600))
				wantError = "not a directory"
			case "runner unavailable":
				f.s.options.reviewChecks = nil
				wantError = "candidate check runner is unavailable"
			case "superseded":
				moveUnitNow(t, f, reopened, stream, "resume", UnitImplementing)
				wantStatus, wantError = ChecksSuperseded, "not checking"
			}
			_, err = c.Apply(context.Background(), op)
			must(t, err)
			run := onlyRun(t, reopened, stream)
			if run.Status != wantStatus || !strings.Contains(run.Error, wantError) {
				t.Fatalf("terminal result: %+v", run)
			}
			if run.Selection == nil || !reflect.DeepEqual(start.Selection, *run.Selection) || !start.StartedAt.Equal(run.StartedAt) || run.DiffSHA256 != start.DiffSHA256 || run.Operation != start.Operation {
				t.Fatalf("terminal result lost original start: %+v, originally %+v", run, start)
			}
			if got := streamDocuments(t, reopened, stream, checkSelectionDocument("resume", 1)); !reflect.DeepEqual(before, got) {
				t.Fatalf("restart duplicated or rewrote selection: %+v", got)
			}
			if len(provider.Requests()) != 1 || len(checks.entered) != 0 {
				t.Fatal("terminal retry rejudged or ran checks")
			}
		})
	}
}

func TestLegacyUnitCheckSelectionStillMovesToReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := newChecksFixture(t, "legacy-selection", &fakeChecks{Outcomes: passing()})
	c := &checkers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	must(t, c.Pass(ctx))
	report := currentReport(t, repo, stream, "resume")
	// This is the selection shape written before listed totals were kept.
	selection := `{"mode":"selected","links":["dag+check://go/test"],"candidates":37,"judgment":"judgment-legacy"}`
	var run UnitCheckRun
	must(t, json.Unmarshal([]byte(fmt.Sprintf(`{"unit":"resume","run":1,"candidate":%q,"base":%q,"report":1,"status":"passed","selection":%s}`, report.Candidate, report.Base, selection)), &run))
	_, err := c.record(ctx, checkInput{Workstream: stream, Unit: "resume", Run: 1}, run, "")
	must(t, err)
	decoded := onlyRun(t, repo, stream)
	if decoded.Selection.Listed != nil || decoded.Selection.Candidates != 37 || decoded.Selection.Mode != selectionSelected {
		t.Fatalf("legacy selection: %+v", decoded.Selection)
	}
	must(t, c.Pass(ctx))
	if state := unitState(t, repo, stream, "resume"); state != UnitReviewing {
		t.Fatalf("legacy checks left unit %s", state)
	}
}

func finalCheckStarts(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []FinalCheckAttempt {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	var starts []FinalCheckAttempt
	for _, doc := range docs {
		var start FinalCheckAttempt
		if json.Unmarshal([]byte(doc.Content), &start) == nil && start.Attempt > 0 && doc.ID == finalCheckAttemptDocument(start.Review, start.Attempt) {
			starts = append(starts, start)
		}
	}
	return starts
}

func TestFinalCheckAttemptsStartAfterPreparationAndRetryDelay(t *testing.T) {
	t.Parallel()
	f, stream, repo, a := newFinalFixture(t, "attempt-start")
	_, op := assembleBoth(t, f, repo, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	clock := &fixedClock{At: f.clock.Now()}
	f.s.options.Reconciliation.Now = clock.Now
	checks := newHeldChecks()
	checks.links = []string{"dag+check://go/test"}
	provider := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.9)}}}}}
	boostJev(f.s, provider)
	f.s.options.reviewChecks = checks
	preparing, release := make(chan string, 1), make(chan struct{})
	defer close(release)
	f.s.boundary = func(name string) error {
		if name == "final-rebase-replaying" || name == "final-checks-exporting" {
			preparing <- name
			<-release
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Each retry uses admission's committee reservation, including after
	// reopening the trace with an existing judgment and unfinished attempt.
	apply := func(ctx context.Context) <-chan error {
		t.Helper()
		release, admitted := f.s.modelOperationLimit(a.repository, nil)(op)
		if !admitted {
			t.Fatal("review did not acquire committee capacity")
		}
		finished := make(chan error, 1)
		go func() {
			_, err := a.Apply(ctx, op)
			release()
			finished <- err
		}()
		return finished
	}
	finished := apply(ctx)
	for _, point := range []string{"final-rebase-replaying", "final-checks-exporting"} {
		if got := awaitCheck(t, preparing); got != point {
			t.Fatalf("preparation boundary %s, want %s", got, point)
		}
		if starts := finalCheckStarts(t, repo, stream); len(starts) != 0 {
			t.Fatalf("checks started during %s: %+v", point, starts)
		}
		release <- struct{}{}
	}
	if links := awaitCheck(t, checks.entered); !slices.Equal(links, checks.links) {
		t.Fatalf("runner links %v, want %v", links, checks.links)
	}
	starts := finalCheckStarts(t, repo, stream)
	if len(starts) != 1 || starts[0].Attempt != 1 || starts[0].Review != 1 || starts[0].Operation != op.ID || starts[0].Commit == "" || starts[0].StartedAt.IsZero() {
		t.Fatalf("held check start: %+v", starts)
	}
	if docs := streamDocuments(t, repo, stream, finalChecksDocument(1)); len(docs) != 0 {
		t.Fatalf("held checks already have a result: %+v", docs)
	}
	checks.release <- checkOutcome{Result: CheckResult{ExitCode: -1}, Err: errors.New("engine unreachable")}
	if err := awaitCheck(t, finished); err == nil {
		t.Fatal("incomplete check attempt succeeded")
	}
	f.s.boundary = nil
	if len(provider.Requests()) != 1 || starts[0].Selection == nil || starts[0].Selection.Mode != selectionSelected {
		t.Fatalf("initial judgment: %d requests, starts %+v", len(provider.Requests()), starts)
	}
	docs := streamDocuments(t, repo, stream, finalChecksDocument(1))
	var incomplete FinalCheckRun
	if len(docs) != 1 {
		t.Fatalf("incomplete results: %+v", docs)
	}
	must(t, json.Unmarshal([]byte(docs[0].Content), &incomplete))
	if !reflect.DeepEqual(incomplete.Selection, starts[0].Selection) || incomplete.Status != ChecksIncomplete || incomplete.Attempt != starts[0].Attempt {
		t.Fatalf("attempt result: %+v", incomplete)
	}
	// Just before the original ten-minute delay ends, retry records no start.
	clock.Advance(incomplete.FinishedAt.Add(finalCheckDelay(1) - time.Nanosecond).Sub(clock.Now()))
	if err := awaitCheck(t, apply(ctx)); err == nil {
		t.Fatal("retry ran before its delay")
	}
	if got := finalCheckStarts(t, repo, stream); !reflect.DeepEqual(got, starts) {
		t.Fatalf("retry waiting recorded a start: %+v", got)
	}
	clock.Advance(time.Nanosecond)
	finished = apply(ctx)
	if links := awaitCheck(t, checks.entered); !slices.Equal(links, checks.links) {
		t.Fatalf("runner links %v, want %v", links, checks.links)
	}
	starts = finalCheckStarts(t, repo, stream)
	if len(starts) != 2 || starts[1].Attempt != 2 || !starts[1].StartedAt.Equal(clock.Now()) {
		t.Fatalf("retry start: %+v", starts)
	}
	if got := streamDocuments(t, repo, stream, finalChecksDocument(1)); !reflect.DeepEqual(got, docs) {
		t.Fatalf("running retry overwrote incomplete result: %+v", got)
	}
	if len(provider.Requests()) != 1 || !reflect.DeepEqual(starts[0].Selection, starts[1].Selection) {
		t.Fatalf("retry did not reuse judgment: %d requests, starts %+v", len(provider.Requests()), starts)
	}
	// Stop the retry while it is running, then reopen the trace. It keeps
	// a start and no result rather than inheriting the earlier attempt's failure.
	cancel()
	if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted final checks: %v", err)
	}
	must(t, repo.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	a = &finalReviewer{s: f.s, repository: reopened}
	// A fresh judge has no in-memory decision; recovery must use the trace.
	restartedProvider := &jevtest.Provider{}
	boostJev(f.s, restartedProvider)
	if got := finalCheckStarts(t, reopened, stream); !reflect.DeepEqual(got, starts) {
		t.Fatalf("starts lost on restart: %+v", got)
	}
	if observed, err := a.Inspect(context.Background(), op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
		t.Fatalf("interrupted review completed: %+v %v", observed, err)
	}
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Checks completed.", "findings": []any{}})
		return err
	})
	finished = apply(context.Background())
	if links := awaitCheck(t, checks.entered); !slices.Equal(links, checks.links) {
		t.Fatalf("runner links %v, want %v", links, checks.links)
	}
	starts = finalCheckStarts(t, reopened, stream)
	if len(starts) != 3 || starts[2].Attempt != 3 {
		t.Fatalf("restarted attempt: %+v", starts)
	}
	if len(restartedProvider.Requests()) != 0 || !reflect.DeepEqual(starts[0].Selection, starts[2].Selection) {
		t.Fatalf("restart asked Jev again or changed selection: %d requests, starts %+v", len(restartedProvider.Requests()), starts)
	}
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	must(t, awaitCheck(t, finished))
	docs = streamDocuments(t, reopened, stream, finalChecksDocument(1))
	var passed FinalCheckRun
	if len(docs) != 2 {
		t.Fatalf("final results: %+v", docs)
	}
	must(t, json.Unmarshal([]byte(docs[1].Content), &passed))
	if !reflect.DeepEqual(passed.Selection, starts[0].Selection) || passed.Status != ChecksPassed || passed.Attempt != 3 || !passed.StartedAt.Equal(starts[2].StartedAt) {
		t.Fatalf("finished attempt: %+v", passed)
	}
}
