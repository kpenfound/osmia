package execution

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	systemone "github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	checkselect "github.com/kpenfound/osmia/internal/checkselect"
	config "github.com/kpenfound/osmia/internal/config"
	jev "github.com/kpenfound/osmia/internal/jev"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func runChecks(t *testing.T, s *Controller, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &checkers{masons: s.Units(repository)}, CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, repository, stream, op, r)
	})
}

func newChecksFixture(t *testing.T, key string, checks ReviewChecks) (*buildFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f := newBuildFixture(t, key)
	f.s.Checks = checks
	seedChecking(t, f, f.repository, stream, "resume", resumeReport)
	return f, stream, f.repository
}

func boostJev(s *Controller, p *jevtest.Provider) {
	s.Jev = intakefixture.CheckJudge(s.Now, p)
}

var unitState = intakefixture.UnitState

func onlyRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) UnitCheckRun {
	t.Helper()
	return intakefixture.OnlyRun(t, repository, stream, checkRuns)
}

func currentReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) UnitReport {
	t.Helper()
	return intakefixture.CurrentReport[UnitReport](t, repository, stream, unit, reportDocument)
}

func reviewTurn(t *testing.T, f *buildFixture, repository *trace.Repository, stream config.WorkstreamID) trace.TurnRequest {
	t.Helper()
	r := &reviewers{masons: f.controller()}
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
	checks := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-pass", checks)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	report := currentReport(t, repository, stream, "resume")
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksPassed || run.Candidate != report.Candidate || run.Base != report.Base || run.Report != 1 || run.Selection == nil || run.Selection.Mode != checkrun.SelectionFull || run.Selection.Reason != string(jev.ReasonDisabled) || run.Selection.Listed != nil || !slices.Equal(run.Command, []string{"dagger", "check", "--progress=report", "--failfast"}) || run.Output != "== CHECKS ==  ✔ 2 passed\n" {
		t.Fatalf("check run %+v", run)
	}

	if ran := checks.Runs(); len(ran) != 1 || ran[0] != nil || checks.Files[0] != "package trace\n" {
		t.Fatalf("checks ran %q on %q", ran, checks.Files)
	}
	if _, err := os.Stat(checks.Dirs[0]); !errors.Is(err, os.ErrNotExist) {
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
	checks := &fakeChecks{Links: links, Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-jev", checks)
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{
		"check-0": jevtest.Noul(0.92), "check-1": jevtest.Noul(0.55), "check-2": jevtest.Noul(0.45), "check-3": jevtest.Noul(0.02)}}}}}
	boostJev(f.s, p)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	selected := []string{links[2], links[3]}
	if ran := checks.Runs(); len(ran) != 1 || !slices.Equal(ran[0], selected) {
		t.Fatalf("checks ran %q, want %q", ran, selected)
	}
	run := onlyRun(t, repository, stream)
	if run.Selection.Mode != checkrun.SelectionSelected || !slices.Equal(run.Selection.Links, selected) || run.Selection.Candidates != 4 || run.Selection.Listed == nil || *run.Selection.Listed != len(links) || run.Selection.Judgment == "" || !slices.Equal(run.Command, append([]string{"dagger", "check", "--progress=report", "--failfast"}, selected...)) {
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

	c := &checkers{masons: f.controller()}
	ops, err := checkOperations(repository, stream)
	must(t, err)
	g, err := newUnitWorkspaces(f.cfg, repository).Of(stream)
	must(t, err)
	diff, err := g.Diff(context.Background(), run.Base, run.Candidate)
	must(t, err)
	again := c.selectChecks(context.Background(), ops[0].input, t.TempDir(), diff, checks)
	if again.Mode != checkrun.SelectionSelected || !slices.Equal(again.Links, selected) || again.Judgment != run.Selection.Judgment || len(p.Requests()) != 1 {
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
		{"rate limited", &fakeChecks{Links: []string{link, "dag+check://release/version"}}, jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRateLimited, Message: "slow down"}}, string(jev.ReasonRateLimited), true},
		{"no check likely", &fakeChecks{Links: []string{link, "dag+check://release/version"}}, jevtest.Result{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.1), "check-1": jevtest.Noul(0.05)}}}, string(jev.ReasonDeclined), true},
		{"no checks", &fakeChecks{}, jevtest.Result{}, checkrun.SelectionNoChecks, false},
		{"unlisted", &fakeChecks{ListErr: errors.New("engine unreachable")}, jevtest.Result{}, checkrun.SelectionUnlisted, false},
		{"too many checks", &fakeChecks{Links: many}, jevtest.Result{}, checkrun.SelectionTooMany, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.checks.Outcomes = passing()
			f, stream, repository := newChecksFixture(t, "fallback", c.checks)
			p := &jevtest.Provider{Results: []jevtest.Result{c.result}}
			boostJev(f.s, p)
			runChecks(t, f.s, repository, stream)
			if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
				t.Fatalf("unit is %s, want reviewing", got)
			}
			run := onlyRun(t, repository, stream)
			if ran := c.checks.Runs(); len(ran) != 1 || ran[0] != nil || run.Selection.Mode != checkrun.SelectionFull || run.Selection.Reason != c.reason {
				t.Fatalf("checks ran %q with selection %+v, want every check for %s", ran, run.Selection, c.reason)
			}
			if c.checks.ListErr != nil {
				if run.Selection.Listed != nil {
					t.Fatalf("failed listing has a total: %+v", run.Selection)
				}
			} else if run.Selection.Listed == nil || *run.Selection.Listed != len(c.checks.Links) {
				t.Fatalf("fallback lost listed total: %+v", run.Selection)
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
	checks := &fakeChecks{Outcomes: []checkOutcome{{Result: api.CheckResult{ExitCode: 1, Output: rawOutput}}, {Result: api.CheckResult{Output: passedReport}}}}
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
	r := &reviewers{masons: f.controller()}
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

	must(t, r.Pass(context.Background()))
	if again, err := repository.Thread(stream, masonAgent("resume")); err != nil || len(again.Turns) != len(th.Turns) {
		t.Fatalf("mason turns %d, want %d: %v", len(again.Turns), len(th.Turns), err)
	}

	w, _, err := newUnitWorkspaces(f.cfg, repository).open(context.Background(), stream, "resume")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n\n// Fixed.\n"), 0644))
	completeMasonTurn(t, f, repository, stream, "resume", "fixed the chunk")
	m := f.controller()
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
	if len(runs) != 2 || runs[1].Run != 2 || runs[1].Status != ChecksPassed || runs[1].Candidate != revised.Candidate || revised.Candidate == failed.Candidate || checks.Files[1] != "package trace\n\n// Fixed.\n" {
		t.Fatalf("check runs %+v", runs)
	}
}

func TestRepeatedCheckFailuresContestTheUnit(t *testing.T) {
	t.Parallel()
	failing := "dag://go/packages/tests/test?go-package=internal/trace"
	checks := &fakeChecks{Outcomes: []checkOutcome{{Result: api.CheckResult{ExitCode: 1, Output: failedReport(failing)}}}}
	f, stream, repository := newChecksFixture(t, "checks-contest", checks)
	f.cfg.Shed.MaxBounces = 1
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitContested {
		t.Fatalf("unit is %s, want contested", got)
	}
	contest, _, err := masonContest(repository, stream, "resume")
	must(t, err)
	if !bounceContest(contest, "resume") || failedReview(contest, "resume") || !strings.Contains(contest.Reason, "1 send-backs reached shed.max_bounces") {
		t.Fatalf("contest %+v", contest)
	}

	if _, apiErr := f.s.RecordContestedRuling(context.Background(), repository, stream, "resume", ContestedRulingRequest{Decision: "review", Note: "the failing test is a known flake"}, ownerActor); apiErr != nil {
		t.Fatal(apiErr)
	}
	r := &reviewers{masons: f.controller()}
	must(t, r.Pass(context.Background()))
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	req := reviewTurn(t, f, repository, stream)
	if !strings.Contains(req.Prompt, "Check run 1 failed") || !strings.Contains(req.Prompt, "Failed: "+failing) || !strings.Contains(req.Prompt, "known flake") {
		t.Fatalf("review prompt:\n%s", req.Prompt)
	}
	if ran := checks.Runs(); len(ran) != 1 {
		t.Fatalf("the ruling ran the checks again: %q", ran)
	}
}

func TestIncompleteChecksHoldTheUnitAndRunAgain(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{Outcomes: []checkOutcome{{Err: errors.New("engine unreachable")}, {Result: api.CheckResult{ExitCode: 1, Output: "Error: no checks matched\n"}}, {Result: api.CheckResult{Output: passedReport}}}}
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

	runChecks(t, f.s, repository, stream)
	if ran := checks.Runs(); len(ran) != 1 {
		t.Fatalf("checks ran again at once: %q", ran)
	}
	advance := func() {
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(checkRetryDelay + time.Minute)
		f.clock.Mu.Unlock()
	}

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
	checks := &fakeChecks{Outcomes: []checkOutcome{{Block: true}}}
	f, stream, repository := newChecksFixture(t, "checks-timeout", checks)
	f.cfg.Project.ChecksTimeout = "50ms"
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || run.Error != "checks did not finish within checks_timeout 50ms" || unitState(t, repository, stream, "resume") != UnitChecking {
		t.Fatalf("check run %+v", run)
	}
}

func TestStoppedChecksRunAgain(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	checks := &fakeChecks{Outcomes: []checkOutcome{{Cancel: cancel}, {Result: api.CheckResult{Output: passedReport}}}}
	f, stream, repository := newChecksFixture(t, "checks-stopped", checks)
	c := &checkers{masons: f.controller()}
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
	checks := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-superseded", checks)
	c := &checkers{masons: f.controller()}
	must(t, c.Pass(context.Background()))
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resume-returned", Revision: 1, Project: repository.Project(), Workstream: stream, Unit: "resume", At: f.s.Now(), Actor: foremanActor, Cause: reportDocument("resume")}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: UnitChecking, To: UnitImplementing, Reason: "returned to its mason"}})
	must(t, err)
	runChecks(t, f.s, repository, stream)
	if run := onlyRun(t, repository, stream); run.Status != ChecksSuperseded || len(checks.Runs()) != 0 {
		t.Fatalf("check run %+v after %d runs", run, len(checks.Runs()))
	}
}

func TestReviewWithoutAMatchingCheckRunReturnsToChecking(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "checks-recheck", checks)
	runChecks(t, f.s, repository, stream)

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

	r := &reviewers{masons: f.controller()}
	must(t, r.Pass(context.Background()))
	if got := unitState(t, repository, stream, "resume"); got != UnitChecking {
		t.Fatalf("unit is %s, want checking", got)
	}
	if _, err := repository.Thread(stream, reviewerAgent("resume")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a review turn was queued without a check run: %v", err)
	}
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing || len(checks.Runs()) != 2 {
		t.Fatalf("unit is %s after %d check runs", got, len(checks.Runs()))
	}
}
