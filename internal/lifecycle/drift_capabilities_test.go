package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/thread"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func enforcedIsolation(t *testing.T, f *journey.ShedFixture, repository *trace.Repository) *isolation.Turns {
	t.Helper()
	bound, err := journey.Enforce(f.Opts, service.Enforcement{}).Threads(repository, f.S.Cfg)
	fixturekit.Must(t, err)
	dispatcher, ok := bound.(thread.Dispatcher)
	if !ok {
		t.Fatalf("threads reconciler %T", bound)
	}
	return dispatcher.Runner.Turns.(*questions.Turns).Turns.(*execution.VerdictTurns).Turns.(*execution.ReportingTurns).Turns.(*execution.RetriedTurns).Turns.(*isolation.Turns)
}

func TestDriftMasonGrantNamesEveryDriftMasonTool(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-mason-grant", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.MasonRole, Thread: intake.DriftMasonAgent, Turn: lifecycle.DriftResolveTurnID(1, 1)}
	selection, err := turns.Select(ctx, scope)
	fixturekit.Must(t, err)
	if selection.Narrow != nil {
		t.Fatalf("the drift mason's selection narrows the role grant: %+v", selection.Narrow)
	}
	scoped, err := turns.Scoped(ctx, scope)
	fixturekit.Must(t, err)
	var names []string
	for _, tool := range scoped {
		names = append(names, tool.Name)
	}
	names = append(names, "file_read", "file_write")
	slices.Sort(names)
	want := slices.Clone(turns.Grants[intake.MasonRole].Tools)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("drift mason toolset %v, want %v", names, want)
	}
}

func TestDriftReviewerGrantNamesEveryDriftReviewerToolAndReadOnlyCandidateView(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-reviewer-grant", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	records := journey.DriftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.ReviewerRole, Thread: intake.DriftReviewerAgent, Turn: lifecycle.DriftReviewTurnID(1, 1)}
	selection, err := turns.Select(ctx, scope)
	fixturekit.Must(t, err)
	if selection.Narrow != nil {
		t.Fatalf("the drift reviewer's selection narrows the role grant: %+v", selection.Narrow)
	}
	if selection.Workspace.BaseRevision != candidate {
		t.Fatalf("the drift reviewer's view is pinned to %s, not the resolved candidate %s", selection.Workspace.BaseRevision, candidate)
	}
	data, err := os.ReadFile(filepath.Join(selection.Workspace.Directory, "CODEOWNERS"))
	fixturekit.Must(t, err)
	if string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the drift reviewer's view of CODEOWNERS: %q", data)
	}
	if _, err := os.Stat(filepath.Join(selection.Workspace.Directory, ".git")); err == nil {
		t.Fatalf("the drift reviewer's view carries VCS metadata")
	}

	scoped, err := turns.Scoped(ctx, scope)
	fixturekit.Must(t, err)
	var names []string
	for _, tool := range scoped {
		names = append(names, tool.Name)
	}
	names = append(names, "file_read")
	slices.Sort(names)
	want := slices.Clone(turns.Grants[intake.ReviewerRole].Tools)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("drift reviewer toolset %v, want %v", names, want)
	}
	if service.TestMasonGrant.WriteFiles == service.TestReviewerGrant.WriteFiles && service.TestMasonGrant.Execute == service.TestReviewerGrant.Execute {
		t.Fatal("reviewerGrant does not differ from masonGrant on writing or execution")
	}
	if service.TestReviewerGrant.WriteFiles || service.TestReviewerGrant.Execute {
		t.Fatalf("reviewerGrant grants writing or execution: %+v", service.TestReviewerGrant)
	}
}

func TestDriftMasonAskRecordsAManagerQuestionNamingTheDriftRebaseAndDeliversToDriftMasonThread(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-mason-ask", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.MasonRole, Thread: intake.DriftMasonAgent, Turn: lifecycle.DriftResolveTurnID(1, 1)}
	q := journey.ClaimOldestTurn(t, f, repository, stream, intake.DriftMasonAgent)
	scoped, err := turns.Scoped(ctx, scope)
	fixturekit.Must(t, err)
	ask := intakefixture.AskTool(t, scoped, questions.AskTool)
	out, err := ask.Handle(ctx, json.RawMessage(`{"question":"For drift rebase 1, should the resolved CODEOWNERS keep both owners on one line, or two?"}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the drift mason's ask: %s", out)
	}
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftMasonAgent, q, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 1"})

	asked, err := repository.Questions(stream)
	fixturekit.Must(t, err)
	if len(asked) != 1 || asked[0].Asked.AskedBy.ID != intake.DriftMasonAgent || asked[0].Asked.Thread != intake.DriftMasonAgent || asked[0].State != trace.QuestionOpen {
		t.Fatalf("the drift mason's question %+v", asked)
	}
	if !strings.Contains(asked[0].Asked.Question, "drift rebase 1") {
		t.Fatalf("the question does not name the drift rebase: %q", asked[0].Asked.Question)
	}

	journey.ManagerAnswers(t, f, repository, stream, "1", "Keep both owners on one line.", "spec#1")
	th, err := repository.Thread(stream, intake.DriftMasonAgent)
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != questions.TurnID("1") {
		t.Fatalf("the answer did not reach the drift-mason thread: %+v", th.Turns)
	}
	if !strings.Contains(th.Turns[1].Request.Prompt, "Keep both owners on one line.") {
		t.Fatalf("the answer turn's prompt %q", th.Turns[1].Request.Prompt)
	}
}

func restartRepository(t *testing.T, f *journey.ShedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	fixturekit.Must(t, repository.Close())
	reopened, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	return reopened
}

func TestDriftMasonAnswerContinuationResumesTheResolutionWorkspaceThroughDoneAcrossARestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-mason-continuation", "")
	f.S.Cfg.Mason.MaxCleanTurns = 1
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.MasonRole, Thread: intake.DriftMasonAgent, Turn: lifecycle.DriftResolveTurnID(1, 1)}
	q := journey.ClaimOldestTurn(t, f, repository, stream, intake.DriftMasonAgent)
	scoped, err := turns.Scoped(ctx, scope)
	fixturekit.Must(t, err)
	out, err := intakefixture.AskTool(t, scoped, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"For drift rebase 1, should the resolved CODEOWNERS keep both owners on one line, or two?"}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the drift mason's ask: %s", out)
	}
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftMasonAgent, q, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 1"})

	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	journey.ManagerAnswers(t, f, repository, stream, "1", "Keep both owners on one line.", "spec#1")

	repository = restartRepository(t, f, repository)
	d = journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	turns = enforcedIsolation(t, f, repository)
	answerScope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.MasonRole, Thread: intake.DriftMasonAgent, Turn: questions.TurnID("1")}
	answer := journey.ClaimOldestTurn(t, f, repository, stream, intake.DriftMasonAgent)
	if answer.Request.TurnID != questions.TurnID("1") {
		t.Fatalf("the oldest unfinished drift-mason turn is %s, not the answer turn", answer.Request.TurnID)
	}
	lease, err := turns.Workspaces.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: answerScope, Access: coreadapter.ReadWrite})
	fixturekit.Must(t, err)
	if lease.Workspace.Directory != w.Path {
		t.Fatalf("the answer turn resolves to workspace %s, not the resolution workspace %s", lease.Workspace.Directory, w.Path)
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	scoped, err = turns.Scoped(ctx, answerScope)
	fixturekit.Must(t, err)
	out, err = intakefixture.AskTool(t, scoped, execution.DoneTool).Handle(ctx, json.RawMessage(`{"outcome":"Kept both owners on one line, as the manager answered"}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the answer turn's done call: %s", out)
	}
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftMasonAgent, answer, journey.ResolvedDone("Kept both owners on one line, as the manager answered"))

	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), questions.TurnID("1")}) {
		t.Fatalf("drift mason turns %v", ids)
	}
}

func TestDriftReviewerAnswerContinuationSeesCandidateAndDiffsAndCompletesTheReviewAcrossARestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-reviewer-continuation", "")
	f.S.Cfg.Shed.MaxBounces = 1
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	before, upstream, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	records := journey.DriftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate

	turns := enforcedIsolation(t, f, repository)
	reviewScope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.ReviewerRole, Thread: intake.DriftReviewerAgent, Turn: lifecycle.DriftReviewTurnID(1, 1)}
	review := journey.ClaimOldestTurn(t, f, repository, stream, intake.DriftReviewerAgent)
	scoped, err := turns.Scoped(ctx, reviewScope)
	fixturekit.Must(t, err)
	out, err := intakefixture.AskTool(t, scoped, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Is the CODEOWNERS line format fixed for drift rebase 1?"}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the drift reviewer's ask: %s", out)
	}
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftReviewerAgent, review, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 1"})

	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	journey.ManagerAnswers(t, f, repository, stream, "1", "Yes, one line.", "spec#1")

	answer1Scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.ReviewerRole, Thread: intake.DriftReviewerAgent, Turn: questions.TurnID("1")}
	answer1 := journey.ClaimOldestTurn(t, f, repository, stream, intake.DriftReviewerAgent)
	if answer1.Request.TurnID != questions.TurnID("1") {
		t.Fatalf("the oldest unfinished drift-reviewer turn is %s, not the first answer turn", answer1.Request.TurnID)
	}
	scoped, err = turns.Scoped(ctx, answer1Scope)
	fixturekit.Must(t, err)
	out, err = intakefixture.AskTool(t, scoped, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Should the drift reviewer also check dedupe.go for drift rebase 1?"}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the answer turn's second ask: %s", out)
	}
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftReviewerAgent, answer1, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 2"})
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	journey.ManagerAnswers(t, f, repository, stream, "2", "No, only CODEOWNERS.", "spec#1")

	asked, err := repository.Questions(stream)
	fixturekit.Must(t, err)
	if len(asked) != 2 || asked[1].Asked.Thread != intake.DriftReviewerAgent || asked[1].Asked.Turn != questions.TurnID("1") {
		t.Fatalf("the second ask did not chain back to the first answer turn: %+v", asked)
	}

	repository = restartRepository(t, f, repository)
	d = journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	turns = enforcedIsolation(t, f, repository)
	answer2Scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.ReviewerRole, Thread: intake.DriftReviewerAgent, Turn: questions.TurnID("2")}
	answer2 := journey.ClaimOldestTurn(t, f, repository, stream, intake.DriftReviewerAgent)
	if answer2.Request.TurnID != questions.TurnID("2") {
		t.Fatalf("the oldest unfinished drift-reviewer turn is %s, not the second answer turn", answer2.Request.TurnID)
	}
	selection, err := turns.Select(ctx, answer2Scope)
	fixturekit.Must(t, err)
	if selection.Workspace.BaseRevision != candidate {
		t.Fatalf("the second answer turn's view is pinned to %s, not the resolved candidate %s", selection.Workspace.BaseRevision, candidate)
	}
	data, err := os.ReadFile(filepath.Join(selection.Workspace.Directory, "CODEOWNERS"))
	fixturekit.Must(t, err)
	if string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the second answer turn's view of CODEOWNERS: %q", data)
	}
	scoped, err = turns.Scoped(ctx, answer2Scope)
	fixturekit.Must(t, err)
	diff := intakefixture.AskTool(t, scoped, reviewtools.DiffToolName)
	for _, change := range []string{"feature", "resolution"} {
		out, err := diff.Handle(ctx, json.RawMessage(fmt.Sprintf(`{"change":%q}`, change)))
		fixturekit.Must(t, err)
		var got struct{ Diff string }
		fixturekit.Must(t, json.Unmarshal(out, &got))
		if strings.TrimSpace(got.Diff) == "" {
			t.Fatalf("the second answer turn's %q diff is empty", change)
		}
	}
	verdict := intakefixture.AskTool(t, scoped, execution.VerdictTool)
	out, err = verdict.Handle(ctx, json.RawMessage(`{"decision":"satisfactory","summary":"Both owners kept, dedupe.go untouched as the manager confirmed.","findings":[]}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the second answer turn's verdict: %s", out)
	}
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftReviewerAgent, answer2, journey.DriftVerdict(t, journey.ApprovedResolution))

	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the approval was applied", tip)
	}
	result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != candidate || journey.FileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
	after := intakefixture.Seals(t, repository, stream)
	if len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after the approved drift rebase %+v", after)
	}
}

func TestDriftPromptsAndSystemPromptsStateTheScopeRule(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-scope-rule", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	conflicted := journey.DriftRecords(t, repository, stream)[len(journey.DriftRecords(t, repository, stream))-1]

	resolve, err := (lifecycle.NewDrifter(d.S.Lifecycle(), d.Cfg, d.Repository)).ResolvePrompt(ctx, stream, conflicted)
	fixturekit.Must(t, err)
	if !strings.Contains(resolve, lifecycle.DriftScopeRule) {
		t.Fatalf("the resolve prompt lacks the scope rule:\n%s", resolve)
	}

	w := journey.ResolutionWorkspace(t, f, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	resolved := journey.DriftRecords(t, repository, stream)[len(journey.DriftRecords(t, repository, stream))-1]

	rejected := resolved
	rejected.Outcome, rejected.Verdict = intake.DriftRejected, &rejectedResolution
	fix, err := (lifecycle.NewDrifter(d.S.Lifecycle(), d.Cfg, d.Repository)).FixPrompt(stream, rejected)
	fixturekit.Must(t, err)
	if !strings.Contains(fix, lifecycle.DriftScopeRule) {
		t.Fatalf("the fix prompt lacks the scope rule:\n%s", fix)
	}

	review, err := lifecycle.NewDrifter(d.S.Lifecycle(), d.Cfg, d.Repository).ReviewPrompt(ctx, stream, resolved)
	fixturekit.Must(t, err)
	for _, want := range []string{lifecycle.DriftScopeRule, "both sides are kept", "lost or altered", "beyond what the rebase needs", "no conflict marker remains", "sealed criteria still hold"} {
		if !strings.Contains(review, want) {
			t.Fatalf("the review prompt lacks %q:\n%s", want, review)
		}
	}

	masonPrompt := lifecycle.DriftMasonSystemPrompt(f.S.Cfg.Project)
	if !strings.Contains(masonPrompt, lifecycle.DriftScopeRule) || !strings.Contains(masonPrompt, questions.AskTool) {
		t.Fatalf("the drift mason's system prompt lacks the scope rule or ask: %s", masonPrompt)
	}
	reviewerPrompt := lifecycle.DriftReviewerSystemPrompt()
	if !strings.Contains(reviewerPrompt, lifecycle.DriftScopeRule) || !strings.Contains(reviewerPrompt, questions.AskTool) {
		t.Fatalf("the drift reviewer's system prompt lacks the scope rule or ask: %s", reviewerPrompt)
	}
}
