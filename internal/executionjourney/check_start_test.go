package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/checkselect"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

type heldChecks struct {
	links   []string
	entered chan []string
	release chan checkOutcome
}

func newHeldChecks() *heldChecks {
	return &heldChecks{entered: make(chan []string, 1), release: make(chan checkOutcome, 1)}
}

func (c *heldChecks) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return c.links, nil
}

func (c *heldChecks) Check(ctx context.Context, _ string, _ api.CheckIdentity, links []string) (CheckResult, error) {
	return intakefixture.HeldCheck(ctx, links, c.entered, c.release)
}

func awaitCheck[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return intakefixture.AwaitCheck(t, ch)
}

func applyCheck(ctx context.Context, c coreadapter.Reconciler, op coreadapter.Operation) <-chan error {
	return intakefixture.ApplyCheck(ctx, c, op)
}

func TestUnitCheckSelectionIsReadableBeforeChecksFinish(t *testing.T) {
	t.Parallel()
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			t.Parallel()
			checks := newHeldChecks()

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
				if selection.Mode != checkrun.SelectionFull || selection.Reason != string(jev.ReasonDeclined) || selection.Detail == "" || len(ran) != 0 {
					t.Fatalf("full selection %+v", selection)
				}
			} else if selection.Mode != checkrun.SelectionSelected || len(ran) != 1 {
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

	selection := `{"mode":"selected","links":["dag+check://go/test"],"candidates":37,"judgment":"judgment-legacy"}`
	var run UnitCheckRun
	must(t, json.Unmarshal([]byte(fmt.Sprintf(`{"unit":"resume","run":1,"candidate":%q,"base":%q,"report":1,"status":"passed","selection":%s}`, report.Candidate, report.Base, selection)), &run))
	_, err := c.record(ctx, checkInput{Workstream: stream, Unit: "resume", Run: 1}, run, "")
	must(t, err)
	decoded := onlyRun(t, repo, stream)
	if decoded.Selection.Listed != nil || decoded.Selection.Candidates != 37 || decoded.Selection.Mode != checkrun.SelectionSelected {
		t.Fatalf("legacy selection: %+v", decoded.Selection)
	}
	must(t, c.Pass(ctx))
	if state := unitState(t, repo, stream, "resume"); state != UnitReviewing {
		t.Fatalf("legacy checks left unit %s", state)
	}
}
