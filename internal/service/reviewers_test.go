package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reviewSummary is how a fixture reviewer says it verified the acceptance.
const reviewSummary = "The candidate does the task and its acceptance holds"

func TestStaleReviewIdentifiesEachRevision(t *testing.T) {
	t.Parallel()
	current := UnitReviewIdentity{Candidate: coreadapter.Candidate{Revision: "candidate", BaseRevision: "base", SpecRevision: "1", PlanRevision: "2"}, DiffSHA256: "diff", Report: "report", Seal: 1}
	for _, tc := range []struct {
		name   string
		change func(*UnitReviewIdentity)
	}{
		{"candidate", func(i *UnitReviewIdentity) { i.Candidate.Revision = "old" }},
		{"base", func(i *UnitReviewIdentity) { i.Candidate.BaseRevision = "old" }},
		{"spec", func(i *UnitReviewIdentity) { i.Candidate.SpecRevision = "old" }},
		{"plan", func(i *UnitReviewIdentity) { i.Candidate.PlanRevision = "old" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewed := current
			tc.change(&reviewed)
			if got := staleReview(reviewed, current); !strings.Contains(got, tc.name) {
				t.Fatal(got)
			}
		})
	}
}

func TestReviewerVerdictToolAndOutcome(t *testing.T) {
	t.Parallel()
	scope := coreadapter.Scope{Workstream: "stream", Unit: "resume", Thread: "reviewer-resume", Turn: "review-1", Role: reviewerRole}
	reports := &reviewerReports{}
	tool := reports.tool(scope)
	for _, input := range []UnitVerdict{{Decision: "satisfactory"}, {Decision: "material_findings", Summary: reviewSummary}} {
		data, _ := json.Marshal(input)
		out, err := tool.Handle(context.Background(), data)
		if err != nil || !strings.Contains(string(out), `"recorded":false`) {
			t.Fatalf("accepted incomplete verdict %s: %s %v", data, out, err)
		}
	}
	good := UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}}
	data, _ := json.Marshal(good)
	out, err := tool.Handle(context.Background(), data)
	if err != nil || !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("verdict refused: %s %v", out, err)
	}
	turns := &verdictTurns{Turns: staticVerdictTurn{}, reports: reports}
	result, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: scope})
	if err != nil || result.Outcome == nil || result.Outcome.Status != verdictOutcome {
		t.Fatalf("outcome %+v %v", result.Outcome, err)
	}
	var got UnitVerdict
	if err := json.Unmarshal([]byte(result.Outcome.Report), &got); err != nil || got.Findings[0].Action != good.Findings[0].Action || got.Summary != reviewSummary {
		t.Fatalf("report %+v %v", got, err)
	}
}

func TestValidateVerdictNamesTheProblem(t *testing.T) {
	t.Parallel()
	finding := ReviewFinding{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}
	for _, tc := range []struct {
		name    string
		verdict UnitVerdict
		want    string
	}{
		{"satisfactory", UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}, ""},
		{"material findings", UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{finding}}, ""},
		{"unknown decision", UnitVerdict{Decision: "approve", Summary: reviewSummary}, "decision must be satisfactory or material_findings"},
		{"no summary", UnitVerdict{Decision: "satisfactory", Summary: " "}, "summary is required"},
		{"material without findings", UnitVerdict{Decision: "material_findings", Summary: reviewSummary}, "material findings are required"},
		{"satisfactory with findings", UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []ReviewFinding{finding}}, "satisfactory verdict cannot carry material findings"},
		{"finding without action", UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "The retry fails"}}}, "each finding needs severity, evidence and action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := validateVerdict(tc.verdict)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// A completed review turn whose verdict the service refuses sends the unit
// back through review with the reason, rather than holding it in reviewing.
func TestRefusedVerdictReviewsTheCandidateAgain(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "refused-verdict")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	must(t, r.one(ctx, stream, "resume", state, false))
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("review turns: %+v", th.Turns)
	}
	// The reviewer verifies the unit's task and acceptance; the review
	// carries the unit's task alone, not the plan's other units.
	if prompt := th.Turns[0].Request.Prompt; !strings.Contains(prompt, "unit resume:\n## Task\n") || !strings.Contains(prompt, "## Acceptance\n- ") || strings.Contains(prompt, "plan.json:") {
		t.Fatalf("review prompt does not carry the unit's task alone:\n%s", prompt)
	}
	if prompt := th.Turns[0].Request.Prompt; !strings.Contains(prompt, "- "+masonWrote+" (+1 -0)\n") || strings.Contains(prompt, "+package trace") {
		t.Fatalf("review prompt carries the diff instead of its files:\n%s", prompt)
	}
	bad, err := json.Marshal(UnitVerdict{Decision: "satisfactory", Summary: " ", Findings: []ReviewFinding{}})
	must(t, err)
	captureTurn(t, f, repo, stream, reviewerAgent("resume"), &coreadapter.Outcome{Status: verdictOutcome, Report: string(bad)})

	for range 2 {
		must(t, r.one(ctx, stream, "resume", state, false))
	}
	refreshed, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if refreshed.Value != UnitReviewing || refreshed.Version != state.Version+1 {
		t.Fatalf("refused verdict left %+v, want reviewing at version %d", refreshed, state.Version+1)
	}
	if _, ok, err := r.storedResult(stream, "resume", refreshed); err != nil || ok {
		t.Fatalf("refused verdict was stored: %v", err)
	}
	must(t, r.one(ctx, stream, "resume", refreshed, false))
	must(t, r.one(ctx, stream, "resume", refreshed, false))
	th, err = repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != reviewTurnID("resume", refreshed.Version) {
		t.Fatalf("review turns after refusal: %+v", th.Turns)
	}
	if prompt := th.Turns[1].Request.Prompt; !strings.Contains(prompt, "was refused: summary is required: say how you verified the unit's acceptance") {
		t.Fatalf("fresh review lacks the refusal: %s", prompt)
	}
}

// staticVerdictTurn is a fake model session that always completes with no
// output; it never exercises a real reviewer's reasoning, so this test only
// checks that verdictTurns.Run reports the verdict already recorded through
// the tool, not that a model session actually produces one.
type staticVerdictTurn struct{}

func (staticVerdictTurn) Run(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{}, nil
}

func TestReviewResultReconcilesAfterRestart(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "review-recovery")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	if err != nil {
		t.Fatal(err)
	}
	turn := reviewTurnID("resume", state.Version)
	result := UnitReviewResult{Identity: identity, Turn: turn, Verdict: UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}}
	data, _ := json.Marshal(result)
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			latest = d
		}
	}
	latest.Revision++
	latest.Content = string(data) + "\n"
	latest.At = time.Now()
	latest.Cause = turn
	if err := repo.RecordDocuments(context.Background(), []trace.Document{latest}); err != nil {
		t.Fatal(err)
	}
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	r = &reviewers{masons: newMasonController(f.s, repo)}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	approved, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil || approved.Value != UnitApproved {
		t.Fatalf("recovered %+v %v", approved, err)
	}
}

func TestStaleCandidateReturnsUnitToReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "stale-review")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*UnitReviewIdentity)
	}{
		{"candidate", func(i *UnitReviewIdentity) { i.Candidate.Revision = i.Candidate.BaseRevision }},
		{"base", func(i *UnitReviewIdentity) { i.Candidate.BaseRevision = i.Candidate.Revision }},
		{"spec", func(i *UnitReviewIdentity) { i.Candidate.SpecRevision = "0" }},
		{"plan", func(i *UnitReviewIdentity) { i.Candidate.PlanRevision = "0" }},
	} {
		reviewed := identity
		tc.change(&reviewed)
		verdict := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
		if tc.name == "plan" {
			verdict.Summary = ""
		}
		result := UnitReviewResult{Identity: reviewed, Turn: reviewTurnID("resume", state.Version), Verdict: verdict}
		if err := r.applyReview(context.Background(), stream, "resume", state, result); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		current, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		if err != nil || current.Value != UnitReviewing || current.Version != state.Version+1 {
			t.Fatalf("%s: stale state %+v: %v", tc.name, current, err)
		}
		transitions, err := trace.Read[trace.Transition](repo, stream)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(transitions[len(transitions)-1].Reason, "stale "+tc.name) {
			t.Fatalf("%s: reason: %s", tc.name, transitions[len(transitions)-1].Reason)
		}
		// Re-entering review starts the count of refused reviews over.
		moveUnitNow(t, f, repo, stream, "resume", UnitChecking)
		moveUnitNow(t, f, repo, stream, "resume", UnitReviewing)
		if state, err = repo.Workflow(stream, trace.UnitSubject("resume")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInterruptedReviewQueuesOneContinuation(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	stopped := false
	defer func() {
		if !stopped {
			f.stop(t)
		}
	}()
	masons.play[masonTurnID("resume")] = reportDone("Built")
	entered := make(chan struct{})
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	stream := f.seedBuilding(t, "interrupted-review", independentPlan)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("review turn did not start")
	}
	f.stop(t)
	stopped = true
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].CompletedAt != (time.Time{}) {
		t.Fatalf("recovery turns: %+v", th.Turns)
	}
}

func TestReviewerQuestionResumesSameCandidateAfterOwnerAnswer(t *testing.T) {
	t.Parallel()
	for _, answerVerdict := range []bool{true, false} {
		t.Run(fmt.Sprintf("answer-verdict-%t", answerVerdict), func(t *testing.T) {
			p := &faults{}
			f, masons, chief := newAskingMasonFixture(t, 1, independentPlan, p)
			defer f.stop(t)
			masons.play[masonTurnID("resume")] = reportDone("Built")
			var reviews atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			f.engine.mu.Lock()
			f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
					if reviews.Add(1) > 1 {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						if !strings.Contains(req.Prompt, relayedRuling) {
							return nil, fmt.Errorf("missing ruling in fresh review")
						}
						body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
						if err != nil || !strings.Contains(body, `"recorded":true`) {
							return nil, fmt.Errorf("verdict %s: %v", body, err)
						}
						return &agent.Result{ClaudeID: "reviewed", ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
					}
					if err := asks(p, "1")(ctx, req, turn, tools); err != nil {
						return nil, err
					}
					return &agent.Result{ClaudeID: "asked", ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
				}
				return chief.turn(ctx, req, turn, tools)
			}
			f.engine.mu.Unlock()
			f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				if !strings.Contains(req.Prompt, relayedRuling) {
					return fmt.Errorf("missing ruling: %s", req.Prompt)
				}
				if !answerVerdict {
					return nil
				}
				body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("verdict %s: %v", body, err)
				}
				return nil
			})
			stream := f.seedBuilding(t, "review-question", independentPlan)
			f.awaitUnit(t, stream, "resume", UnitWaiting)
			docs, err := trace.Read[trace.Document](f.repository(), stream)
			must(t, err)
			var before UnitReviewIdentity
			for _, d := range docs {
				if d.ID == reviewDocument("resume") {
					_ = json.Unmarshal([]byte(d.Content), &before)
				}
			}
			if before.Candidate.Revision == "" {
				t.Fatal("review identity was not recorded")
			}
			f.stop(t)
			f.start(t)
			f.awaitUnit(t, stream, "resume", UnitWaiting)
			f.rule(t, "1")
			if !answerVerdict {
				select {
				case <-started:
				case <-time.After(demoTimeout):
					t.Fatal("no fresh review after answer")
				}
				state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
				must(t, err)
				if state.Value != UnitReviewing {
					t.Fatalf("answer alone moved unit to %s", state.Value)
				}
				close(release)
			}
			f.awaitUnit(t, stream, "resume", UnitMerged)
			r := &reviewers{masons: newMasonController(f.s, f.repository())}
			result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitApproved})
			must(t, err)
			if !ok || result.Identity.Candidate.Revision != before.Candidate.Revision || (answerVerdict && result.Turn != questions.TurnID("1")) {
				t.Fatalf("review after answer: %+v", result)
			}
			th := f.thread(t, stream, reviewerAgent("resume"))
			wantTurns := 2
			if !answerVerdict {
				wantTurns++
			}
			if len(th.Turns) != wantTurns || th.Turns[0].Status() != questions.Waiting {
				t.Fatalf("reviewer turns: %+v", th.Turns)
			}
			p.check(t)
			masons.check(t)
		})
	}
}

func TestContestedReviewRulingSurvivesRestart(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	f.s.cfg.Shed.MaxBounces = 1
	masons.play[masonTurnID("resume")] = reportDone("Built")
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "material_findings", "summary": reviewSummary, "findings": []ReviewFinding{{Severity: "material", Evidence: "Retry fails", Action: "Handle retry"}}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "reviewed", ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	stream := f.seedBuilding(t, "contested", independentPlan)
	f.awaitUnit(t, stream, "resume", UnitContested)
	r := &reviewers{masons: newMasonController(f.s, f.repository())}
	result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitContested})
	must(t, err)
	if !ok || result.Bounces != 1 {
		t.Fatalf("bounce count: %+v", result)
	}
	raiseFixtureContest(t, f.repository(), stream, "resume")
	status, err := f.c.Status(context.Background(), stream)
	if err != nil || !slices.Contains(status.Gates, trace.OwnerGate{Kind: UnitContested, Reference: "resume"}) {
		t.Fatalf("contested gate: %+v %v", status.Gates, err)
	}
	if entries := f.raisedContests(t, stream); len(entries) != 1 || entries[0].Unit != "resume" || !slices.Equal(entries[0].Options, []string{"review", "revise"}) ||
		entries[0].Answer.Path != "/v1/contested/"+string(stream)+"/resume" {
		t.Fatalf("inbox entries of the contested unit %+v", entries)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Check the candidate once more"); err != nil {
		t.Fatal(err)
	}
	// The ruled unit leaves the inbox, whether or not it has resumed yet.
	if entries := f.inboxEntries(t, stream, InboxContested); len(entries) != 0 {
		t.Fatalf("inbox entries after the ruling %+v", entries)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Decide twice"); err == nil {
		t.Fatal("accepted duplicate ruling")
	}
	deadline := time.Now().Add(demoTimeout)
	for {
		result, ok, err = r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitContested})
		must(t, err)
		state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
		must(t, err)
		if ok && result.Bounces == 2 && state.Value == UnitContested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second review did not contest: %+v", result)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "The mason should fix the retry proof"); err != nil {
		t.Fatal(err)
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", UnitImplementing)
	deadline = time.Now().Add(demoTimeout)
	var th trace.Thread
	for {
		th = f.thread(t, stream, masonAgent("resume"))
		if len(th.Turns) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("revision turn was not queued: %+v", th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(th.Turns) != 2 || (!strings.Contains(th.Turns[1].Request.Prompt, "Handle retry") || !strings.Contains(th.Turns[1].Request.Prompt, "The mason should fix the retry proof")) {
		t.Fatalf("revision turn: %+v", th.Turns)
	}
	if _, found, err := latestContestedRuling(f.repository(), stream, "resume", 2); err != nil || !found {
		t.Fatalf("ruling lost: %v %v", found, err)
	}
	masons.check(t)
}

// A reviewing unit whose workspace a landing left behind the feature branch
// gets no review turn and no refreshed review: a review of its candidate
// would be refused as stale, so it waits for the foreman's rebase.
func TestReviewWaitsForTheRebaseOfAUnitLeftBehind(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, "review-behind", independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	seedReview(t, f, repository, stream, "dedupe", dedupeReport)
	approveDirectly(t, f.s, repository, stream, "resume", "spec#1")
	lands := &foreman{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	must(t, lands.Pass(ctx))
	landings := landOperations(t, repository, stream)
	if len(landings) != 1 {
		t.Fatalf("landing operations %+v", landings)
	}
	if result := settleOperation(t, f.s, repository, stream, landings[0].Operation, lands); result.Outcome != "succeeded" {
		t.Fatalf("landing result %+v", result)
	}
	units := newUnitWorkspaces(f.s.cfg, repository)
	if behind, err := units.behind(ctx, stream, "dedupe"); err != nil || !behind {
		t.Fatalf("dedupe is not behind the landed feature branch: %v %v", behind, err)
	}

	r := &reviewers{masons: lands.masons}
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	if state.Value != UnitReviewing {
		t.Fatalf("dedupe is %s, not reviewing", state.Value)
	}
	for range 2 {
		must(t, r.one(ctx, stream, "dedupe", state, false))
	}
	if th, err := repository.Thread(stream, reviewerAgent("dedupe")); err == nil && len(th.Turns) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the candidate left behind was offered for review: %+v %v", th.Turns, err)
	}
	if after, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || after != state {
		t.Fatalf("dedupe moved from %+v to %+v while it waits for its rebase: %v", state, after, err)
	}

	must(t, lands.Pass(ctx))
	rebases := rebaseOperations(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("dedupe's rebases %+v", rebases)
	}
	settleOperation(t, f.s, repository, stream, rebases[0].Operation, rebaser{lands})
	if behind, err := units.behind(ctx, stream, "dedupe"); err != nil || behind {
		t.Fatalf("dedupe is behind after its rebase: %v %v", behind, err)
	}
}

// A unit whose reviews the service refuses maxReviewRefreshes times in a row
// is contested instead of reviewed again, as a failed review whose ruling is
// another review, and the chief of staff hears why.
func TestRepeatedlyRefusedReviewsContestTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "refresh-limit")
	r := &reviewers{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	for i := 1; i < maxReviewRefreshes; i++ {
		state, err := repository.Workflow(stream, subject)
		must(t, err)
		must(t, r.refreshReview(ctx, stream, "resume", state, "stale base revision; review the current candidate again"))
		if after, err := repository.Workflow(stream, subject); err != nil || after.Value != UnitReviewing {
			t.Fatalf("refused review %d left the unit %+v: %v", i, after, err)
		}
	}
	state, err := repository.Workflow(stream, subject)
	must(t, err)
	must(t, r.refreshReview(ctx, stream, "resume", state, "stale base revision; review the current candidate again"))
	contested, err := repository.Workflow(stream, subject)
	must(t, err)
	if contested.Value != UnitContested {
		t.Fatalf("refused review %d left the unit %s", maxReviewRefreshes, contested.Value)
	}
	transitions := allTransitions(t, f.trace, stream)
	contest := transitions[len(transitions)-1]
	if contest.Subject != subject || !failedReview(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("refused %d reviews of unit resume in a row", maxReviewRefreshes)) {
		t.Fatalf("the contest %+v", contest)
	}
	if opts := contestOptions(contest, "resume", false); !slices.Equal(opts, []string{"review"}) {
		t.Fatalf("the contest offers %v", opts)
	}
}

// interruptTurn runs the latest turn of the workstream's agent as a turn a
// stop interrupted.
func interruptTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	must(t, err)
	token := "stopped-" + th.Turns[len(th.Turns)-1].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, t.TempDir(), f.clock.Now())
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Cancelled: true}}
	must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.clock.Now()))
	return q
}

// A review whose turns are interrupted maxRecoveries times in a row is not
// continued again: the unit is contested as a failed review.
func TestRepeatedlyInterruptedReviewContestsTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "interrupted-review")
	r := &reviewers{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	state, err := repository.Workflow(stream, subject)
	must(t, err)
	must(t, r.one(ctx, stream, "resume", state, false))
	for i := 1; i <= maxRecoveries; i++ {
		interruptTurn(t, f, repository, stream, reviewerAgent("resume"))
		must(t, r.one(ctx, stream, "resume", state, false))
		got, err := repository.Workflow(stream, subject)
		must(t, err)
		if i < maxRecoveries && got.Value != UnitReviewing || i == maxRecoveries && got.Value != UnitContested {
			t.Fatalf("after %d interruptions the unit is %s", i, got.Value)
		}
	}
	if ids := turnIDs(t, repository, stream, reviewerAgent("resume")); len(ids) != maxRecoveries {
		t.Fatalf("reviewer turns %v", ids)
	}
	transitions := allTransitions(t, f.trace, stream)
	contest := transitions[len(transitions)-1]
	if !failedReview(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("the latest %d reviewer turns of unit resume were interrupted in a row", maxRecoveries)) {
		t.Fatalf("the contest %+v", contest)
	}

	// The chief of staff hands the unit back to its reviewer: the review its
	// move asks for starts a new count, so an interruption of it is continued.
	if _, api := f.s.moveUnit(ctx, repository, stream, "resume", UnitMoveRequest{To: UnitReviewing, Note: "The service is stable again; review it."}, chiefActor, ""); api != nil {
		t.Fatalf("the chief's move: %+v", api)
	}
	state, err = repository.Workflow(stream, subject)
	must(t, err)
	must(t, r.one(ctx, stream, "resume", state, false))
	interruptTurn(t, f, repository, stream, reviewerAgent("resume"))
	must(t, r.one(ctx, stream, "resume", state, false))
	if got, err := repository.Workflow(stream, subject); err != nil || got.Value != UnitReviewing {
		t.Fatalf("after the move and one interruption the unit is %+v: %v", got, err)
	}
	ids := turnIDs(t, repository, stream, reviewerAgent("resume"))
	if last := ids[len(ids)-1]; !strings.Contains(last, "-recover-") {
		t.Fatalf("the interrupted review after the move was not continued: %v", ids)
	}
}
