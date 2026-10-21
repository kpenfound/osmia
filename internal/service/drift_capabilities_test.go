package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// enforcedIsolation unwraps the production isolation.Turns Enforce builds
// for repository and cfg from the question, verdict, reporting and retry
// decorators it wraps around it, so a test can call its Select and Scoped
// exactly as the running service does.
func enforcedIsolation(t *testing.T, f *shedFixture, repository *trace.Repository) *isolation.Turns {
	t.Helper()
	bound, err := Enforce(f.opts, Enforcement{}).Threads(repository, f.s.cfg)
	must(t, err)
	dispatcher, ok := bound.(thread.Dispatcher)
	if !ok {
		t.Fatalf("threads reconciler %T", bound)
	}
	return dispatcher.Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
}

// claimOldestTurn claims the thread's oldest unfinished turn and returns it,
// so a test can call its scoped tools exactly as a running turn would.
func claimOldestTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	th, err := repository.Thread(stream, agent)
	must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to claim", agent)
	}
	token := "claim-" + th.Turns[i].Request.TurnID
	directory := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), agent, token)
	q, err := repository.ClaimTurn(context.Background(), stream, agent, token, directory, f.clock.Now())
	must(t, err)
	return q
}

// chiefAnswers has the real chief-of-staff answer tool record a ruling for
// question id, citing cite, through a queued and claimed chief-of-staff
// turn, and delivers it to the asker's thread through the service's own
// Deliverer: the production question path a running service's chief uses.
func chiefAnswers(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	chiefRecordsAnswer(t, f, repository, stream, id, text, cite)
	must(t, f.s.answers(f.s.cfg, repository).Pass(context.Background()))
}

// chiefRecordsAnswer records the production answer without running delivery.
func chiefRecordsAnswer(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	f.sharedTurns().ChiefRecordsAnswer(t, repository, stream, id, text, cite)
}

// askTool returns the named tool of list, failing the test if it is absent.
func askTool(t *testing.T, list []coreadapter.Tool, name string) coreadapter.Tool {
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

// A drift mason turn's production toolset, built through Enforce's Grants,
// Select and Scoped, is exactly masonGrant's: file_read, file_write, ask,
// amend, factory_context, discovery and done.
func TestDriftMasonGrantNamesEveryDriftMasonTool(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-mason-grant")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: masonRole, Thread: driftMasonAgent, Turn: driftResolveTurnID(1, 1)}
	selection, err := turns.Select(ctx, scope)
	must(t, err)
	if selection.Narrow != nil {
		t.Fatalf("the drift mason's selection narrows the role grant: %+v", selection.Narrow)
	}
	scoped, err := turns.Scoped(ctx, scope)
	must(t, err)
	var names []string
	for _, tool := range scoped {
		names = append(names, tool.Name)
	}
	// file_read and file_write come from the view, not Scoped.
	names = append(names, "file_read", "file_write")
	slices.Sort(names)
	want := slices.Clone(turns.Grants[masonRole].Tools)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("drift mason toolset %v, want %v", names, want)
	}
}

// A drift reviewer turn's view holds the resolved candidate's files,
// read-only, and its production toolset, built through Enforce's Grants,
// Select and Scoped, is exactly reviewerGrant's: file_read, ask,
// factory_context, discovery, workstream_diff and verdict. reviewerGrant
// holds neither WriteFiles nor Execute, so the turn writes nothing and
// executes nothing.
func TestDriftReviewerGrantNamesEveryDriftReviewerToolAndReadOnlyCandidateView(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-reviewer-grant")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	records := driftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: reviewerRole, Thread: driftReviewerAgent, Turn: driftReviewTurnID(1, 1)}
	selection, err := turns.Select(ctx, scope)
	must(t, err)
	if selection.Narrow != nil {
		t.Fatalf("the drift reviewer's selection narrows the role grant: %+v", selection.Narrow)
	}
	if selection.Workspace.BaseRevision != candidate {
		t.Fatalf("the drift reviewer's view is pinned to %s, not the resolved candidate %s", selection.Workspace.BaseRevision, candidate)
	}
	data, err := os.ReadFile(filepath.Join(selection.Workspace.Directory, "CODEOWNERS"))
	must(t, err)
	if string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the drift reviewer's view of CODEOWNERS: %q", data)
	}
	if _, err := os.Stat(filepath.Join(selection.Workspace.Directory, ".git")); err == nil {
		t.Fatalf("the drift reviewer's view carries VCS metadata")
	}

	scoped, err := turns.Scoped(ctx, scope)
	must(t, err)
	var names []string
	for _, tool := range scoped {
		names = append(names, tool.Name)
	}
	names = append(names, "file_read")
	slices.Sort(names)
	want := slices.Clone(turns.Grants[reviewerRole].Tools)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("drift reviewer toolset %v, want %v", names, want)
	}
	if masonGrant.WriteFiles == reviewerGrant.WriteFiles && masonGrant.Execute == reviewerGrant.Execute {
		t.Fatal("reviewerGrant does not differ from masonGrant on writing or execution")
	}
	if reviewerGrant.WriteFiles || reviewerGrant.Execute {
		t.Fatalf("reviewerGrant grants writing or execution: %+v", reviewerGrant)
	}
}

// A drift mason's ask is recorded as a question to the workstream's chief
// of staff naming the drift rebase it resolves, and the chief of staff's
// answer, delivered through the production question path, reaches the
// drift-mason thread as its next turn.
func TestDriftMasonAskRecordsAChiefOfStaffQuestionNamingTheDriftRebaseAndDeliversToDriftMasonThread(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-mason-ask")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: masonRole, Thread: driftMasonAgent, Turn: driftResolveTurnID(1, 1)}
	q := claimOldestTurn(t, f, repository, stream, driftMasonAgent)
	scoped, err := turns.Scoped(ctx, scope)
	must(t, err)
	ask := askTool(t, scoped, questions.AskTool)
	out, err := ask.Handle(ctx, json.RawMessage(`{"question":"For drift rebase 1, should the resolved CODEOWNERS keep both owners on one line, or two?"}`))
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the drift mason's ask: %s", out)
	}
	completeClaimedTurn(t, f, repository, stream, driftMasonAgent, q, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 1"})

	asked, err := repository.Questions(stream)
	must(t, err)
	if len(asked) != 1 || asked[0].Asked.AskedBy.ID != driftMasonAgent || asked[0].Asked.Thread != driftMasonAgent || asked[0].State != trace.QuestionOpen {
		t.Fatalf("the drift mason's question %+v", asked)
	}
	if !strings.Contains(asked[0].Asked.Question, "drift rebase 1") {
		t.Fatalf("the question does not name the drift rebase: %q", asked[0].Asked.Question)
	}

	chiefAnswers(t, f, repository, stream, "1", "Keep both owners on one line.", "spec#1")
	th, err := repository.Thread(stream, driftMasonAgent)
	must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != questions.TurnID("1") {
		t.Fatalf("the answer did not reach the drift-mason thread: %+v", th.Turns)
	}
	if !strings.Contains(th.Turns[1].Request.Prompt, "Keep both owners on one line.") {
		t.Fatalf("the answer turn's prompt %q", th.Turns[1].Request.Prompt)
	}
}

// restartRepository closes repository and reopens it, so a test exercises
// durable state exactly as a service restart would.
func restartRepository(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	must(t, repository.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	return reopened
}

// A drift mason waiting on a chief-of-staff answer leaves its drift rebase
// pending, not held, even with mason.max_clean_turns set to 1. The chief of
// staff's answer, delivered through the production question path, reaches
// the drift-mason thread as an answer turn. After the service wiring is
// reconstructed as after a restart, that answer turn resolves to the same
// resolution workspace, and calling the production done tool through it
// completes the resolution and lets it proceed to review.
func TestDriftMasonAnswerContinuationResumesTheResolutionWorkspaceThroughDoneAcrossARestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-mason-continuation")
	f.s.cfg.Mason.MaxCleanTurns = 1
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)

	turns := enforcedIsolation(t, f, repository)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: masonRole, Thread: driftMasonAgent, Turn: driftResolveTurnID(1, 1)}
	q := claimOldestTurn(t, f, repository, stream, driftMasonAgent)
	scoped, err := turns.Scoped(ctx, scope)
	must(t, err)
	out, err := askTool(t, scoped, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"For drift rebase 1, should the resolved CODEOWNERS keep both owners on one line, or two?"}`))
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the drift mason's ask: %s", out)
	}
	completeClaimedTurn(t, f, repository, stream, driftMasonAgent, q, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 1"})

	// The wait leaves the resolution pending, not held, although a single
	// marker reminder would already exhaust mason.max_clean_turns.
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	chiefAnswers(t, f, repository, stream, "1", "Keep both owners on one line.", "spec#1")

	repository = restartRepository(t, f, repository)
	d = drifter{&foreman{masons: newMasonController(f.s, repository)}}
	turns = enforcedIsolation(t, f, repository)
	answerScope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: masonRole, Thread: driftMasonAgent, Turn: questions.TurnID("1")}
	answer := claimOldestTurn(t, f, repository, stream, driftMasonAgent)
	if answer.Request.TurnID != questions.TurnID("1") {
		t.Fatalf("the oldest unfinished drift-mason turn is %s, not the answer turn", answer.Request.TurnID)
	}
	lease, err := turns.Workspaces.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: answerScope, Access: coreadapter.ReadWrite})
	must(t, err)
	if lease.Workspace.Directory != w.Path {
		t.Fatalf("the answer turn resolves to workspace %s, not the resolution workspace %s", lease.Workspace.Directory, w.Path)
	}
	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	scoped, err = turns.Scoped(ctx, answerScope)
	must(t, err)
	out, err = askTool(t, scoped, doneTool).Handle(ctx, json.RawMessage(`{"outcome":"Kept both owners on one line, as the chief of staff answered"}`))
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the answer turn's done call: %s", out)
	}
	completeClaimedTurn(t, f, repository, stream, driftMasonAgent, answer, resolvedDone("Kept both owners on one line, as the chief of staff answered"))

	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1), questions.TurnID("1")}) {
		t.Fatalf("drift mason turns %v", ids)
	}
}

// A drift reviewer waiting on a chief-of-staff answer leaves its drift
// rebase pending, not held, even with shed.max_bounces set to 1. A second
// ask from the answer turn still resolves to the same drift rebase and
// review, and its own answer arrives on the same thread. After the service
// wiring is reconstructed as after a restart, that second answer turn sees
// the resolved candidate's files and both pinned diffs of workstream_diff,
// and calling the production verdict tool through it completes the review
// of the originating drift rebase.
func TestDriftReviewerAnswerContinuationSeesCandidateAndDiffsAndCompletesTheReviewAcrossARestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-reviewer-continuation")
	f.s.cfg.Shed.MaxBounces = 1
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before, upstream, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	records := driftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate

	turns := enforcedIsolation(t, f, repository)
	reviewScope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: reviewerRole, Thread: driftReviewerAgent, Turn: driftReviewTurnID(1, 1)}
	review := claimOldestTurn(t, f, repository, stream, driftReviewerAgent)
	scoped, err := turns.Scoped(ctx, reviewScope)
	must(t, err)
	out, err := askTool(t, scoped, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Is the CODEOWNERS line format fixed for drift rebase 1?"}`))
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the drift reviewer's ask: %s", out)
	}
	completeClaimedTurn(t, f, repository, stream, driftReviewerAgent, review, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 1"})

	// The wait leaves the resolution pending, not held, although a single
	// bounce would already exhaust shed.max_bounces.
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	chiefAnswers(t, f, repository, stream, "1", "Yes, one line.", "spec#1")

	answer1Scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: reviewerRole, Thread: driftReviewerAgent, Turn: questions.TurnID("1")}
	answer1 := claimOldestTurn(t, f, repository, stream, driftReviewerAgent)
	if answer1.Request.TurnID != questions.TurnID("1") {
		t.Fatalf("the oldest unfinished drift-reviewer turn is %s, not the first answer turn", answer1.Request.TurnID)
	}
	scoped, err = turns.Scoped(ctx, answer1Scope)
	must(t, err)
	out, err = askTool(t, scoped, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Should the drift reviewer also check dedupe.go for drift rebase 1?"}`))
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the answer turn's second ask: %s", out)
	}
	completeClaimedTurn(t, f, repository, stream, driftReviewerAgent, answer1, &coreadapter.Outcome{Status: questions.Waiting, Report: "Asked question 2"})
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	chiefAnswers(t, f, repository, stream, "2", "No, only CODEOWNERS.", "spec#1")

	asked, err := repository.Questions(stream)
	must(t, err)
	if len(asked) != 2 || asked[1].Asked.Thread != driftReviewerAgent || asked[1].Asked.Turn != questions.TurnID("1") {
		t.Fatalf("the second ask did not chain back to the first answer turn: %+v", asked)
	}

	repository = restartRepository(t, f, repository)
	d = drifter{&foreman{masons: newMasonController(f.s, repository)}}
	turns = enforcedIsolation(t, f, repository)
	answer2Scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: reviewerRole, Thread: driftReviewerAgent, Turn: questions.TurnID("2")}
	answer2 := claimOldestTurn(t, f, repository, stream, driftReviewerAgent)
	if answer2.Request.TurnID != questions.TurnID("2") {
		t.Fatalf("the oldest unfinished drift-reviewer turn is %s, not the second answer turn", answer2.Request.TurnID)
	}
	selection, err := turns.Select(ctx, answer2Scope)
	must(t, err)
	if selection.Workspace.BaseRevision != candidate {
		t.Fatalf("the second answer turn's view is pinned to %s, not the resolved candidate %s", selection.Workspace.BaseRevision, candidate)
	}
	data, err := os.ReadFile(filepath.Join(selection.Workspace.Directory, "CODEOWNERS"))
	must(t, err)
	if string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the second answer turn's view of CODEOWNERS: %q", data)
	}
	scoped, err = turns.Scoped(ctx, answer2Scope)
	must(t, err)
	diff := askTool(t, scoped, reviewtools.DiffToolName)
	for _, change := range []string{"feature", "resolution"} {
		out, err := diff.Handle(ctx, json.RawMessage(fmt.Sprintf(`{"change":%q}`, change)))
		must(t, err)
		var got struct{ Diff string }
		must(t, json.Unmarshal(out, &got))
		if strings.TrimSpace(got.Diff) == "" {
			t.Fatalf("the second answer turn's %q diff is empty", change)
		}
	}
	verdict := askTool(t, scoped, verdictTool)
	out, err = verdict.Handle(ctx, json.RawMessage(`{"decision":"satisfactory","summary":"Both owners kept, dedupe.go untouched as the chief of staff confirmed.","findings":[]}`))
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the second answer turn's verdict: %s", out)
	}
	completeClaimedTurn(t, f, repository, stream, driftReviewerAgent, answer2, driftVerdict(t, approvedResolution))

	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the approval was applied", tip)
	}
	result, err := attemptOperation(t, f.s, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := featureTip(t, f, stream); tip != candidate || fileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
	after := seals(t, repository, stream)
	if len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after the approved drift rebase %+v", after)
	}
}

// The drift mason's resolve and fix prompts, the drift reviewer's review
// prompt, and both agents' system prompts, state that conflicted paths and
// changes the rebased branch needs are in scope of the drift rebase
// whatever the workstream's spec, plan or footprints name, that a resolved
// path outside that scope is not on its own a reason to reject, and that
// the reviewer checks both sides are kept, the feature change is not lost
// or altered, no change goes beyond what the rebase needs, no marker
// remains and the sealed criteria hold.
func TestDriftPromptsAndSystemPromptsStateTheScopeRule(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-scope-rule")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	conflicted := driftRecords(t, repository, stream)[len(driftRecords(t, repository, stream))-1]

	resolve, err := d.resolvePrompt(ctx, stream, conflicted)
	must(t, err)
	if !strings.Contains(resolve, driftScopeRule) {
		t.Fatalf("the resolve prompt lacks the scope rule:\n%s", resolve)
	}

	w := resolutionWorkspace(t, f, stream)
	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	resolved := driftRecords(t, repository, stream)[len(driftRecords(t, repository, stream))-1]

	rejected := resolved
	rejected.Outcome, rejected.Verdict = driftRejected, &rejectedResolution
	fix, err := d.fixPrompt(stream, rejected)
	must(t, err)
	if !strings.Contains(fix, driftScopeRule) {
		t.Fatalf("the fix prompt lacks the scope rule:\n%s", fix)
	}

	review, err := d.reviewPrompt(ctx, stream, resolved)
	must(t, err)
	for _, want := range []string{driftScopeRule, "both sides are kept", "lost or altered", "beyond what the rebase needs", "no conflict marker remains", "sealed criteria still hold"} {
		if !strings.Contains(review, want) {
			t.Fatalf("the review prompt lacks %q:\n%s", want, review)
		}
	}

	masonPrompt := driftMasonSystemPrompt(f.s.cfg.Project)
	if !strings.Contains(masonPrompt, driftScopeRule) || !strings.Contains(masonPrompt, questions.AskTool) {
		t.Fatalf("the drift mason's system prompt lacks the scope rule or ask: %s", masonPrompt)
	}
	reviewerPrompt := driftReviewerSystemPrompt()
	if !strings.Contains(reviewerPrompt, driftScopeRule) || !strings.Contains(reviewerPrompt, questions.AskTool) {
		t.Fatalf("the drift reviewer's system prompt lacks the scope rule or ask: %s", reviewerPrompt)
	}
}
