package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevtest"
	"github.com/kpenfound/osmia/internal/systemone"
	"github.com/kpenfound/osmia/internal/trace"
)

// runChecks runs the checks controller's passes and reconciles each check
// run they request as the reconciliation controller does, one at a time as
// in a workstream, until none is pending, so each checking unit moves on its
// run's result.
func runChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	c := &checkers{masons: newMasonController(s, repository)}
	for {
		must(t, c.Pass(ctx))
		ops, err := repository.Operations(stream)
		must(t, err)
		i := slices.IndexFunc(ops, func(op trace.OperationRecord) bool { return op.Operation.Action == CheckAction && op.Result == nil })
		if i < 0 {
			return
		}
		_, err = attemptOperation(t, s, repository, stream, ops[i].Operation, c)
		must(t, err)
	}
}

// checkOutcome is one scripted check result. Cancel, when set, is called
// before the check returns, as a service stopping mid-run does. Block waits
// for the check's context to end, as a run that never finishes does.
type checkOutcome struct {
	result CheckResult
	err    error
	cancel context.CancelFunc
	block  bool
}

// fakeChecks lists links and returns its scripted outcomes in order,
// repeating the last, recording the links of each run and the candidate file
// it saw. It stands in for the real dagger check execution boundary,
// leaving the real CLI's link discovery, exit codes and output unverified.
type fakeChecks struct {
	links    []string
	listErr  error
	outcomes []checkOutcome

	mu    sync.Mutex
	ran   [][]string
	dirs  []string
	files []string
}

func (f *fakeChecks) List(context.Context, string) ([]string, error) { return f.links, f.listErr }

func (f *fakeChecks) Check(ctx context.Context, dir string, links []string) (CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := os.ReadFile(filepath.Join(dir, masonWrote))
	f.ran, f.dirs, f.files = append(f.ran, links), append(f.dirs, dir), append(f.files, string(data))
	o := f.outcomes[min(len(f.ran), len(f.outcomes))-1]
	if o.block {
		<-ctx.Done()
		return CheckResult{ExitCode: -1}, ctx.Err()
	}
	if o.cancel != nil {
		o.cancel()
		return CheckResult{ExitCode: -1}, ctx.Err()
	}
	return o.result, o.err
}

func (f *fakeChecks) runs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ran)
}

const passedReport = "== CHECKS ==  ✔ 2 passed\n✔ dag://go/packages/tests/test 2.0s OK\n✔ dag://release/version-round-trip 1.0s OK\n"

func failedReport(link string) string {
	return fmt.Sprintf("== TRACE ==  ✘ FAILED\n\n== CHECKS ==  ✘ 1 failed\n✘ %s 1.0s ERROR\n    ✘ TestBuilt FAIL\n        built_test.go:9: wrong chunk\n", link)
}

func passing() []checkOutcome { return []checkOutcome{{result: CheckResult{Output: passedReport}}} }

// newChecksFixture builds a workstream whose unit resume is checking, with
// checks as the service's check runner.
func newChecksFixture(t *testing.T, key string, checks ReviewChecks) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	f.s.options.reviewChecks = checks
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

// boostJev turns the Jev boost on with p answering.
func boostJev(s *Service, p *jevtest.Provider) {
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "1s"}
	s.jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: s.now}
}

func unitState(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	must(t, err)
	return state.Value
}

func onlyRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) UnitCheckRun {
	t.Helper()
	runs, err := checkRuns(repository, stream)
	must(t, err)
	if len(runs) != 1 {
		t.Fatalf("check runs %+v, want one", runs)
	}
	return runs[0]
}

func currentReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) UnitReport {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	var report UnitReport
	for _, d := range docs {
		if d.ID == reportDocument(unit) {
			must(t, json.Unmarshal([]byte(d.Content), &report))
		}
	}
	return report
}

// reviewTurn passes the reviewers and returns the review turn they queued.
func reviewTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) trace.TurnRequest {
	t.Helper()
	r := &reviewers{masons: newMasonController(f.s, repository)}
	must(t, r.Pass(context.Background()))
	th, err := repository.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	if len(th.Turns) == 0 {
		t.Fatal("no review turn was queued")
	}
	return th.Turns[len(th.Turns)-1].Request
}

func TestPassingChecksHandTheirResultToReview(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-pass", checks)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	report := currentReport(t, repository, stream, "resume")
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksPassed || run.Candidate != report.Candidate || run.Base != report.Base || run.Report != 1 || run.Selection == nil || run.Selection.Mode != selectionFull || run.Selection.Reason != string(jev.ReasonDisabled) || !slices.Equal(run.Command, []string{"dagger", "check", "--progress=report", "--failfast"}) || run.Output != "== CHECKS ==  ✔ 2 passed\n" {
		t.Fatalf("check run %+v", run)
	}
	// The boost is off: every check runs on the exact candidate, in an
	// export removed afterwards, and nothing is asked of Jev.
	if ran := checks.runs(); len(ran) != 1 || ran[0] != nil || checks.files[0] != "package trace\n" {
		t.Fatalf("checks ran %q on %q", ran, checks.files)
	}
	if _, err := os.Stat(checks.dirs[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check export was kept: %v", err)
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	if slices.ContainsFunc(docs, func(d trace.Document) bool { return strings.HasPrefix(d.Path, "judgments/") }) {
		t.Fatal("a judgment was recorded with the boost off")
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	i := slices.IndexFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject == trace.UnitSubject("resume") && tr.To == UnitReviewing
	})
	if i < 0 || transitions[i].From != UnitChecking || transitions[i].Actor != checksActor || transitions[i].Cause != checkDocumentID("resume", 1) || !strings.HasPrefix(transitions[i].ID, trace.UnitSubject("resume")+"-reviewing-checks-1-") {
		t.Fatalf("move to review %+v", transitions)
	}

	req := reviewTurn(t, f, repository, stream)
	if !strings.Contains(req.Prompt, "Check run 1 passed: every check ran: the Jev boost is off.") || !strings.Contains(req.Prompt, run.Output) || !strings.Contains(req.Prompt, run.OutputPath) {
		t.Fatalf("review prompt lacks the check run:\n%s", req.Prompt)
	}
}

func TestChecksRunTheLinksJevSelects(t *testing.T) {
	t.Parallel()
	links := []string{
		"dag+check://release/version-round-trip",
		"dag+check://go/packages/tests/test?go-package=internal/web&go-test=TestPage",
		"dag+check://go/packages/tests/test?go-package=internal/trace&go-test=TestBuilt",
		"dag+check://go/packages/tests/test?go-package=internal/trace&go-test=TestChunks",
	}
	checks := &fakeChecks{links: links, outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-jev", checks)
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{
		"check-0": jevtest.Noul(0.92), "check-1": jevtest.Noul(0.55), "check-2": jevtest.Noul(0.45), "check-3": jevtest.Noul(0.02)}}}}}
	boostJev(f.s, p)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	selected := []string{links[2], links[3]}
	if ran := checks.runs(); len(ran) != 1 || !slices.Equal(ran[0], selected) {
		t.Fatalf("checks ran %q, want %q", ran, selected)
	}
	run := onlyRun(t, repository, stream)
	if run.Selection.Mode != selectionSelected || !slices.Equal(run.Selection.Links, selected) || run.Selection.Candidates != 4 || run.Selection.Judgment == "" || !slices.Equal(run.Command, append([]string{"dagger", "check", "--progress=report", "--failfast"}, selected...)) {
		t.Fatalf("check run %+v", run)
	}
	requests := p.Requests()
	if len(requests) != 1 || len(requests[0].Questions) != 4 || !strings.Contains(fmt.Sprint(requests[0].Questions["check-0"].Instructions), links[2]) {
		t.Fatalf("Jev requests %+v", requests)
	}
	state, _ := requests[0].State.(map[string]any)
	if files, _ := state["changed_files"].(string); !strings.Contains(files, masonWrote) || state["diff"] == nil {
		t.Fatalf("judgment state %+v", state)
	}
	if req := reviewTurn(t, f, repository, stream); !strings.Contains(req.Prompt, "Jev selected 2 of 4 check links for the changed files in judgment "+run.Selection.Judgment) {
		t.Fatalf("review prompt lacks the selection:\n%s", req.Prompt)
	}

	// The selection is recorded: asked again for the same run, after a
	// restart, it is read back rather than asked again.
	c := &checkers{masons: newMasonController(f.s, repository)}
	ops, err := checkOperations(repository, stream)
	must(t, err)
	g, err := newUnitWorkspaces(f.s.cfg, repository).of(stream)
	must(t, err)
	diff, err := g.Diff(context.Background(), run.Base, run.Candidate)
	must(t, err)
	again := c.selectChecks(context.Background(), ops[0].input, t.TempDir(), diff, checks)
	if again.Mode != selectionSelected || !slices.Equal(again.Links, selected) || again.Judgment != run.Selection.Judgment || len(p.Requests()) != 1 {
		t.Fatalf("the recorded selection was not reused: %+v after %d requests", again, len(p.Requests()))
	}
}

func TestChecksFallBackToEveryCheck(t *testing.T) {
	t.Parallel()
	many := make([]string, checkselect.MaxQuestions+1)
	for i := range many {
		many[i] = fmt.Sprintf("dag+check://module-%03d/check", i)
	}
	link := "dag+check://go/packages/tests/test?go-package=internal/trace"
	for _, c := range []struct {
		name   string
		checks *fakeChecks
		result jevtest.Result
		reason string
		asked  bool
	}{
		{"rate limited", &fakeChecks{links: []string{link, "dag+check://release/version"}}, jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRateLimited, Message: "slow down"}}, string(jev.ReasonRateLimited), true},
		{"no check likely", &fakeChecks{links: []string{link, "dag+check://release/version"}}, jevtest.Result{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.1), "check-1": jevtest.Noul(0.05)}}}, string(jev.ReasonDeclined), true},
		{"unlisted", &fakeChecks{listErr: errors.New("engine unreachable")}, jevtest.Result{}, selectionUnlisted, false},
		{"too many checks", &fakeChecks{links: many}, jevtest.Result{}, selectionTooMany, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.checks.outcomes = passing()
			f, stream, repository := newChecksFixture(t, "fallback", c.checks)
			p := &jevtest.Provider{Results: []jevtest.Result{c.result}}
			boostJev(f.s, p)
			runChecks(t, f.s, repository, stream)
			if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
				t.Fatalf("unit is %s, want reviewing", got)
			}
			run := onlyRun(t, repository, stream)
			if ran := c.checks.runs(); len(ran) != 1 || ran[0] != nil || run.Selection.Mode != selectionFull || run.Selection.Reason != c.reason {
				t.Fatalf("checks ran %q with selection %+v, want every check for %s", ran, run.Selection, c.reason)
			}
			if asked := len(p.Requests()) > 0; asked != c.asked {
				t.Fatalf("Jev asked: %t, want %t", asked, c.asked)
			}
		})
	}
}

func TestFailedChecksReturnTheUnitToItsMason(t *testing.T) {
	t.Parallel()
	failing := "dag://go/packages/tests/test?go-package=internal/trace"
	rawOutput := failedReport(failing) + "✔ dag://passing 1s OK\n" + strings.Repeat("    passing detail\n", 5000) + "RUN LOCALLY\n" + strings.Repeat("rerun command ", 10000)
	checks := &fakeChecks{outcomes: []checkOutcome{{result: CheckResult{ExitCode: 1, Output: rawOutput}}, {result: CheckResult{Output: passedReport}}}}
	f, stream, repository := newChecksFixture(t, "checks-fail", checks)
	failed := currentReport(t, repository, stream, "resume")
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitImplementing {
		t.Fatalf("unit is %s, want implementing", got)
	}
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksFailed || !slices.Equal(run.Failed, []string{failing}) || run.ExitCode != 1 {
		t.Fatalf("check run %+v", run)
	}
	outputDocs := streamDocuments(t, repository, stream, checkDocumentID("resume", 1)+"-output")
	if len(outputDocs) != 1 || outputDocs[0].Content != rawOutput || outputDocs[0].Path != run.OutputPath || run.OutputPath != "units/resume/checks-1-output.txt" || strings.Contains(run.Output, "passing detail") {
		t.Fatal("complete check output or extracted diagnostics were not recorded")
	}
	r := &reviewers{masons: newMasonController(f.s, repository)}
	result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitImplementing})
	must(t, err)
	if !ok || result.Checks != 1 || result.Bounces != 1 || result.Verdict.Decision != "material_findings" || len(result.Verdict.Findings) != 1 || !strings.Contains(result.Verdict.Findings[0].Evidence, failing) || result.Identity.Candidate.Revision != failed.Candidate {
		t.Fatalf("send-back %+v", result)
	}
	th, err := repository.Thread(stream, masonAgent("resume"))
	must(t, err)
	revise := th.Turns[len(th.Turns)-1].Request
	if revise.TurnID != masonAgent("resume")+"-revise-checks-1" || revise.Actor != checksActor || !strings.Contains(revise.Prompt, "The project's checks failed on candidate "+failed.Candidate) || !strings.Contains(revise.Prompt, "built_test.go:9: wrong chunk") || !strings.Contains(revise.Prompt, run.OutputPath) || strings.Contains(revise.Prompt, "passing detail") {
		t.Fatalf("revise turn %+v", revise)
	}
	// The reviewers' pass does not queue the send-back twice.
	must(t, r.Pass(context.Background()))
	if again, err := repository.Thread(stream, masonAgent("resume")); err != nil || len(again.Turns) != len(th.Turns) {
		t.Fatalf("mason turns %d, want %d: %v", len(again.Turns), len(th.Turns), err)
	}

	// The revised candidate is a new candidate: its checks run afresh, and
	// the failed run of the old one does not decide it.
	w, _, err := newUnitWorkspaces(f.s.cfg, repository).open(context.Background(), stream, "resume")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n\n// Fixed.\n"), 0644))
	completeMasonTurn(t, f, repository, stream, "resume", "fixed the chunk")
	m := newMasonController(f.s, repository)
	b, _, err := m.read(stream)
	must(t, err)
	if moved, blocked, err := m.finish(context.Background(), b, "resume"); err != nil || !moved || blocked {
		t.Fatalf("revised unit did not reach checking: %t %t %v", moved, blocked, err)
	}
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	runs, err := checkRuns(repository, stream)
	must(t, err)
	revised := currentReport(t, repository, stream, "resume")
	if len(runs) != 2 || runs[1].Run != 2 || runs[1].Status != ChecksPassed || runs[1].Candidate != revised.Candidate || revised.Candidate == failed.Candidate || checks.files[1] != "package trace\n\n// Fixed.\n" {
		t.Fatalf("check runs %+v", runs)
	}
}

func TestRepeatedCheckFailuresContestTheUnit(t *testing.T) {
	t.Parallel()
	failing := "dag://go/packages/tests/test?go-package=internal/trace"
	checks := &fakeChecks{outcomes: []checkOutcome{{result: CheckResult{ExitCode: 1, Output: failedReport(failing)}}}}
	f, stream, repository := newChecksFixture(t, "checks-contest", checks)
	f.s.cfg.Shed.MaxBounces = 1
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitContested {
		t.Fatalf("unit is %s, want contested", got)
	}
	contest, _, err := masonContest(repository, stream, "resume")
	must(t, err)
	if !bounceContest(contest, "resume") || failedReview(contest, "resume") || !strings.Contains(contest.Reason, "1 send-backs reached shed.max_bounces") {
		t.Fatalf("contest %+v", contest)
	}

	// The owner rules that the candidate is reviewed despite the failure:
	// its reviewer receives the failed run as evidence.
	if _, apiErr := f.s.recordContestedRuling(context.Background(), repository, stream, "resume", ContestedRulingRequest{Decision: "review", Note: "the failing test is a known flake"}, ownerActor); apiErr != nil {
		t.Fatal(apiErr)
	}
	r := &reviewers{masons: newMasonController(f.s, repository)}
	must(t, r.Pass(context.Background()))
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	req := reviewTurn(t, f, repository, stream)
	if !strings.Contains(req.Prompt, "Check run 1 failed") || !strings.Contains(req.Prompt, "Failed: "+failing) || !strings.Contains(req.Prompt, "known flake") {
		t.Fatalf("review prompt:\n%s", req.Prompt)
	}
	if ran := checks.runs(); len(ran) != 1 {
		t.Fatalf("the ruling ran the checks again: %q", ran)
	}
}

func TestIncompleteChecksHoldTheUnitAndRunAgain(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{outcomes: []checkOutcome{{err: errors.New("engine unreachable")}, {result: CheckResult{ExitCode: 1, Output: "Error: no checks matched\n"}}, {result: CheckResult{Output: passedReport}}}}
	f, stream, repository := newChecksFixture(t, "checks-incomplete", checks)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitChecking {
		t.Fatalf("unit is %s, want checking", got)
	}
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || run.Error != "engine unreachable" {
		t.Fatalf("check run %+v", run)
	}
	subject, err := repository.Workflow(stream, checksSubject("resume"))
	must(t, err)
	if subject.Value != "incomplete-1" {
		t.Fatalf("checks subject %+v", subject)
	}
	outbox, err := repository.Outbox(stream)
	must(t, err)
	i := slices.IndexFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == checkDocumentID("resume", 1)+"-incomplete" && e.Event.Kind == trace.NoticeKind
	})
	if i < 0 || !strings.Contains(outbox[i].Event.Body, "held in checking") || !strings.Contains(outbox[i].Event.Body, "engine unreachable") {
		t.Fatalf("chief notices %+v", outbox)
	}

	// The checks wait before they run again.
	runChecks(t, f.s, repository, stream)
	if ran := checks.runs(); len(ran) != 1 {
		t.Fatalf("checks ran again at once: %q", ran)
	}
	advance := func() {
		f.clock.mu.Lock()
		f.clock.now = f.clock.now.Add(checkRetryDelay + time.Minute)
		f.clock.mu.Unlock()
	}
	// A run that exits without reporting a failed check did not complete.
	advance()
	runChecks(t, f.s, repository, stream)
	runs, err := checkRuns(repository, stream)
	must(t, err)
	if len(runs) != 2 || runs[1].Status != ChecksIncomplete || !strings.Contains(runs[1].Error, "without reporting a failed check") || unitState(t, repository, stream, "resume") != UnitChecking {
		t.Fatalf("check runs %+v", runs)
	}
	advance()
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
}

func TestChecksOutlastingTheirTimeoutAreIncomplete(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{outcomes: []checkOutcome{{block: true}}}
	f, stream, repository := newChecksFixture(t, "checks-timeout", checks)
	f.s.cfg.Project.ChecksTimeout = "50ms"
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || run.Error != "checks did not finish within checks_timeout 50ms" || unitState(t, repository, stream, "resume") != UnitChecking {
		t.Fatalf("check run %+v", run)
	}
}

func TestStoppedChecksRunAgain(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	checks := &fakeChecks{outcomes: []checkOutcome{{cancel: cancel}, {result: CheckResult{Output: passedReport}}}}
	f, stream, repository := newChecksFixture(t, "checks-stopped", checks)
	c := &checkers{masons: newMasonController(f.s, repository)}
	must(t, c.Pass(context.Background()))
	ops, err := repository.Operations(stream)
	must(t, err)
	i := slices.IndexFunc(ops, func(op trace.OperationRecord) bool { return op.Operation.Action == CheckAction })
	if _, err := c.Apply(ctx, ops[i].Operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped run returned %v", err)
	}
	if runs, err := checkRuns(repository, stream); err != nil || len(runs) != 0 {
		t.Fatalf("a stopped run was recorded: %+v %v", runs, err)
	}
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing || onlyRun(t, repository, stream).Run != 1 {
		t.Fatalf("unit is %s after the run was resumed", got)
	}
}

func TestChecksOfAReplacedCandidateDoNotRun(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-superseded", checks)
	c := &checkers{masons: newMasonController(f.s, repository)}
	must(t, c.Pass(context.Background()))
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resume-returned", Revision: 1, Project: repository.Project(), Workstream: stream, Unit: "resume", At: f.s.now(), Actor: foremanActor, Cause: reportDocument("resume")}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: UnitChecking, To: UnitImplementing, Reason: "returned to its mason"}})
	must(t, err)
	runChecks(t, f.s, repository, stream)
	if run := onlyRun(t, repository, stream); run.Status != ChecksSuperseded || len(checks.runs()) != 0 {
		t.Fatalf("check run %+v after %d runs", run, len(checks.runs()))
	}
}

func TestReviewWithoutAMatchingCheckRunReturnsToChecking(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-recheck", checks)
	runChecks(t, f.s, repository, stream)
	// The recorded run no longer matches the unit's candidate.
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == checkDocumentID("resume", 1) })
	var run UnitCheckRun
	must(t, json.Unmarshal([]byte(docs[i].Content), &run))
	run.Candidate = strings.Repeat("0", 40)
	data, err := json.Marshal(run)
	must(t, err)
	doc := docs[i]
	doc.Revision, doc.Content = 2, string(data)
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{doc}))

	r := &reviewers{masons: newMasonController(f.s, repository)}
	must(t, r.Pass(context.Background()))
	if got := unitState(t, repository, stream, "resume"); got != UnitChecking {
		t.Fatalf("unit is %s, want checking", got)
	}
	if _, err := repository.Thread(stream, reviewerAgent("resume")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a review turn was queued without a check run: %v", err)
	}
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing || len(checks.runs()) != 2 {
		t.Fatalf("unit is %s after %d check runs", got, len(checks.runs()))
	}
}

// A unit whose check runs of one candidate fail to complete
// maxIncompleteChecks times in a row is contested instead of its checks
// running again. A ruling of review runs them again before review.
func TestIncompleteChecksAreBoundedAndContestTheUnit(t *testing.T) {
	t.Parallel()
	var outcomes []checkOutcome
	for range maxIncompleteChecks {
		outcomes = append(outcomes, checkOutcome{err: errors.New("engine unreachable")})
	}
	checks := &fakeChecks{outcomes: append(outcomes, checkOutcome{result: CheckResult{Output: passedReport}})}
	f, stream, repository := newChecksFixture(t, "checks-bounded", checks)
	ctx := context.Background()
	advance := func() {
		f.clock.mu.Lock()
		f.clock.now = f.clock.now.Add(checkRetryDelay + time.Minute)
		f.clock.mu.Unlock()
	}
	for range maxIncompleteChecks {
		runChecks(t, f.s, repository, stream)
		advance()
	}
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitContested {
		t.Fatalf("unit is %s after %d incomplete runs", got, maxIncompleteChecks)
	}
	if ran := checks.runs(); len(ran) != maxIncompleteChecks {
		t.Fatalf("checks ran %d times", len(ran))
	}
	contest := transitionByID(t, repository, stream, checkDocumentID("resume", maxIncompleteChecks)+"-contested")
	if !checksContest(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("%d check runs in a row", maxIncompleteChecks)) || !strings.Contains(contest.Reason, "engine unreachable") {
		t.Fatalf("the contest %+v", contest)
	}
	advance()
	runChecks(t, f.s, repository, stream)
	if ran := checks.runs(); len(ran) != maxIncompleteChecks {
		t.Fatalf("a contested unit's checks ran again: %d runs", len(ran))
	}

	must(t, repository.Close())
	f.start(t)
	defer f.stop(t)
	if _, api := f.s.ruleContested(ctx, string(stream), "resume", ContestedRulingRequest{Decision: "review", Note: "The engine is back."}); api != nil {
		t.Fatalf("ruling review: %+v", api)
	}
	deadline := time.Now().Add(demoTimeout)
	for {
		runs, err := checkRuns(f.repository(), stream)
		must(t, err)
		if len(runs) == maxIncompleteChecks+1 && runs[maxIncompleteChecks].Status == ChecksPassed && unitState(t, f.repository(), stream, "resume") == UnitReviewing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the ruling the check runs are %+v and the unit is %s", runs, unitState(t, f.repository(), stream, "resume"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
