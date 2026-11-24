package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type heldChecks struct {
	links   []string
	entered chan []string
	release chan fixturekit.CheckOutcome
}

func newHeldChecks() *heldChecks {
	return &heldChecks{entered: make(chan []string, 1), release: make(chan fixturekit.CheckOutcome, 1)}
}

func (c *heldChecks) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return c.links, nil
}

func (c *heldChecks) Check(ctx context.Context, _ string, _ api.CheckIdentity, links []string) (api.CheckResult, error) {
	return intakefixture.HeldCheck(ctx, links, c.entered, c.release)
}

func awaitCheck[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return intakefixture.AwaitCheck(t, ch)
}

func TestUnitCheckSelectionIsReadableBeforeChecksFinish(t *testing.T) {
	t.Parallel()
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			t.Parallel()
			checker := newHeldChecks()

			for i := range checkselect.MaxQuestions + 1 {
				checker.links = append(checker.links, fmt.Sprintf("dag+check://go/packages/tests/test?go-package=internal/trace&go-test=Test%d", i))
			}
			checker.links[len(checker.links)-1] = "dag+check://go/packages/tests/test?go-package=internal/zzz&go-test=TestOther"
			f, stream, repo := newChecksFixture(t, "selection-start", checker)
			probability := 0.9
			if full {
				probability = 0.1
			}
			provider := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(probability), "check-1": jevtest.Noul(.1)}}}}}
			useJevProvider(f.s, provider)
			c := &checkers{masons: newMasonController(f.s, repo)}
			fixturekit.Must(t, c.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == service.CheckAction })
			op := ops[i].Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := intakefixture.ApplyCheck(ctx, c, op)
			ran := awaitCheck(t, checker.entered)
			docs := intakefixture.StreamDocuments(t, repo, stream, execution.CheckSelectionDocument("resume", 1))
			if len(docs) != 1 {
				t.Fatalf("selection records while running: %+v", docs)
			}
			var start execution.UnitCheckSelection
			fixturekit.Must(t, json.Unmarshal([]byte(docs[0].Content), &start))
			selection := start.Selection
			if selection.Listed == nil || *selection.Listed != len(checker.links) || selection.Candidates != 2 || selection.Judgment == "" || !slices.Equal(selection.Links, ran) {
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
			state, err := repo.Workflow(stream, execution.ChecksSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value != "requested-1" || intakefixture.UnitState(t, repo, stream, "resume") != execution.UnitChecking {
				t.Fatalf("start changed workflow: %+v", state)
			}
			checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
			fixturekit.Must(t, awaitCheck(t, finished))
			run := onlyRun(t, repo, stream)
			if run.Status != checkrun.ChecksPassed || !reflect.DeepEqual(*run.Selection, selection) || !run.StartedAt.Equal(start.StartedAt) {
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
			checker := newHeldChecks()
			checker.links = []string{"dag+check://go/test"}
			f, stream, repo := newChecksFixture(t, "selection-restart", checker)
			p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.9)}}}}}
			if !full {
				useJevProvider(f.s, p)
			}
			c := &checkers{masons: newMasonController(f.s, repo)}
			fixturekit.Must(t, c.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == service.CheckAction })
			op := ops[i].Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := intakefixture.ApplyCheck(ctx, c, op)
			firstLinks := awaitCheck(t, checker.entered)
			before := intakefixture.StreamDocuments(t, repo, stream, execution.CheckSelectionDocument("resume", 1))
			cancel()
			if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run: %v", err)
			}
			fixturekit.Must(t, repo.Close())
			reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			fixturekit.Must(t, err)
			t.Cleanup(func() { reopened.Close() })
			c = &checkers{masons: newMasonController(f.s, reopened)}
			if runs, err := execution.CheckRuns(reopened, stream); err != nil || len(runs) != 0 {
				t.Fatalf("interrupted run has a result: %+v %v", runs, err)
			}
			if got := intakefixture.StreamDocuments(t, reopened, stream, execution.CheckSelectionDocument("resume", 1)); len(before) != 1 || !reflect.DeepEqual(before, got) {
				t.Fatalf("start lost on restart: %+v", got)
			}
			if observed, err := c.Inspect(context.Background(), op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
				t.Fatalf("interrupted run completed: %+v %v", observed, err)
			}

			checker.links = []string{"dag+check://different/test"}
			useJevProvider(f.s, &jevtest.Provider{})
			finished = intakefixture.ApplyCheck(context.Background(), c, op)
			if links := awaitCheck(t, checker.entered); !slices.Equal(firstLinks, links) {
				t.Fatalf("restart changed check links: %v, originally %v", links, firstLinks)
			}
			checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
			fixturekit.Must(t, awaitCheck(t, finished))
			if got := intakefixture.StreamDocuments(t, reopened, stream, execution.CheckSelectionDocument("resume", 1)); !reflect.DeepEqual(before, got) {
				t.Fatalf("restart duplicated or rewrote selection: %+v", got)
			}
			var start execution.UnitCheckSelection
			fixturekit.Must(t, json.Unmarshal([]byte(before[0].Content), &start))
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
			checker := newHeldChecks()
			checker.links = []string{"dag+check://go/test"}
			f, stream, repo := newChecksFixture(t, "selection-terminal-restart", checker)
			probability := 0.9
			if terminal == "runner unavailable" {
				probability = 0.1
			}
			provider := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(probability)}}}}}
			useJevProvider(f.s, provider)
			c := &checkers{masons: newMasonController(f.s, repo)}
			fixturekit.Must(t, c.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == service.CheckAction })
			op := ops[i].Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := intakefixture.ApplyCheck(ctx, c, op)
			awaitCheck(t, checker.entered)
			before := intakefixture.StreamDocuments(t, repo, stream, execution.CheckSelectionDocument("resume", 1))
			if len(before) != 1 {
				t.Fatalf("running selection records: %+v", before)
			}
			var start execution.UnitCheckSelection
			fixturekit.Must(t, json.Unmarshal([]byte(before[0].Content), &start))
			if start.Selection.Listed == nil || *start.Selection.Listed != len(checker.links) {
				t.Fatalf("running selection lost listed total: %+v", start.Selection)
			}
			cancel()
			if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run: %v", err)
			}
			fixturekit.Must(t, repo.Close())
			reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			fixturekit.Must(t, err)
			t.Cleanup(func() { reopened.Close() })
			c = &checkers{masons: newMasonController(f.s, reopened)}
			if observed, err := c.Inspect(context.Background(), op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
				t.Fatalf("interrupted run completed: %+v %v", observed, err)
			}
			wantStatus, wantError := checkrun.ChecksIncomplete, ""
			switch terminal {
			case "preparation failure":

				base := filepath.Join(f.s.cfg.Root.String(), "checks")
				fixturekit.Must(t, os.RemoveAll(base))
				fixturekit.Must(t, os.WriteFile(base, []byte("blocks export preparation"), 0600))
				wantError = "not a directory"
			case "runner unavailable":
				f.s.options.reviewChecks = nil
				wantError = "candidate check runner is unavailable"
			case "superseded":
				moveUnitNow(t, f, reopened, stream, "resume", intake.UnitImplementing)
				wantStatus, wantError = checkrun.ChecksSuperseded, "not checking"
			}
			_, err = c.Apply(context.Background(), op)
			fixturekit.Must(t, err)
			run := onlyRun(t, reopened, stream)
			if run.Status != wantStatus || !strings.Contains(run.Error, wantError) {
				t.Fatalf("terminal result: %+v", run)
			}
			if run.Selection == nil || !reflect.DeepEqual(start.Selection, *run.Selection) || !start.StartedAt.Equal(run.StartedAt) || run.DiffSHA256 != start.DiffSHA256 || run.Operation != start.Operation {
				t.Fatalf("terminal result lost original start: %+v, originally %+v", run, start)
			}
			if got := intakefixture.StreamDocuments(t, reopened, stream, execution.CheckSelectionDocument("resume", 1)); !reflect.DeepEqual(before, got) {
				t.Fatalf("restart duplicated or rewrote selection: %+v", got)
			}
			if len(provider.Requests()) != 1 || len(checker.entered) != 0 {
				t.Fatal("terminal retry rejudged or ran checks")
			}
		})
	}
}

func TestUnitCheckSelectionWithoutAListedTotalMovesToReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := newChecksFixture(t, "selection-without-listed", &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()})
	c := &checkers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	fixturekit.Must(t, c.Pass(ctx))
	report := currentReport(t, repo, stream, "resume")

	selection := `{"mode":"selected","links":["dag+check://go/test"],"candidates":37,"judgment":"judgment-recorded"}`
	var run execution.UnitCheckRun
	fixturekit.Must(t, json.Unmarshal([]byte(fmt.Sprintf(`{"unit":"resume","run":1,"candidate":%q,"base":%q,"report":1,"status":"passed","selection":%s}`, report.Candidate, report.Base, selection)), &run))
	_, err := c.record(ctx, execution.CheckInput{Workstream: stream, Unit: "resume", Run: 1}, run, "")
	fixturekit.Must(t, err)
	decoded := onlyRun(t, repo, stream)
	if decoded.Selection.Listed != nil || decoded.Selection.Candidates != 37 || decoded.Selection.Mode != checkrun.SelectionSelected {
		t.Fatalf("selection: %+v", decoded.Selection)
	}
	fixturekit.Must(t, c.Pass(ctx))
	if state := intakefixture.UnitState(t, repo, stream, "resume"); state != intake.UnitReviewing {
		t.Fatalf("checks left unit %s", state)
	}
}
