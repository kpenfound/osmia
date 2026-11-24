package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	os "os"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const reviewSummary = "The candidate does the task and its acceptance holds"

func TestRefusedVerdictReviewsTheCandidateAgain(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "refused-verdict")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	th, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("review turns: %+v", th.Turns)
	}
	intakefixture.AssertCheckGateReviewInstructions(t, th.Turns[0].Request.SystemPrompt)

	if prompt := th.Turns[0].Request.Prompt; !strings.Contains(prompt, "unit resume:\n## Task\n") || !strings.Contains(prompt, "## Acceptance\n- ") || strings.Contains(prompt, "plan.json:") {
		t.Fatalf("review prompt does not carry the unit's task alone:\n%s", prompt)
	}
	if prompt := th.Turns[0].Request.Prompt; !strings.Contains(prompt, "- "+fixturekit.MasonWrote+" (+1 -0)\n") || strings.Contains(prompt, "+package trace") {
		t.Fatalf("review prompt carries the diff instead of its files:\n%s", prompt)
	}
	bad, err := json.Marshal(service.UnitVerdict{Decision: "satisfactory", Summary: " ", Findings: []api.ReviewFinding{}})
	fixturekit.Must(t, err)
	captureTurn(t, f, repo, stream, intake.ReviewerAgent("resume"), &coreadapter.Outcome{Status: execution.VerdictOutcome, Report: string(bad)})

	for range 2 {
		fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	}
	refreshed, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if refreshed.Value != intake.UnitReviewing || refreshed.Version != state.Version+1 {
		t.Fatalf("refused verdict left %+v, want reviewing at version %d", refreshed, state.Version+1)
	}
	if _, ok, err := r.storedResult(stream, "resume", refreshed); err != nil || ok {
		t.Fatalf("refused verdict was stored: %v", err)
	}
	fixturekit.Must(t, r.one(ctx, stream, "resume", refreshed, false))
	fixturekit.Must(t, r.one(ctx, stream, "resume", refreshed, false))
	th, err = repo.Thread(stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != service.TestReviewTurnID("resume", refreshed.Version) {
		t.Fatalf("review turns after refusal: %+v", th.Turns)
	}
	if prompt := th.Turns[1].Request.Prompt; !strings.Contains(prompt, "was refused: summary is required: say how you verified the unit's acceptance") {
		t.Fatalf("fresh review lacks the refusal: %s", prompt)
	}
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
	turn := service.TestReviewTurnID("resume", state.Version)
	result := service.UnitReviewResult{Identity: identity, Turn: turn, Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}}
	data, _ := json.Marshal(result)
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == service.TestReviewDocument("resume") {
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
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	r = &reviewers{masons: newMasonController(f.s, repo)}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	approved, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil || approved.Value != intake.UnitApproved {
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
		change func(*service.UnitReviewIdentity)
	}{
		{"candidate", func(i *service.UnitReviewIdentity) { i.Candidate.Revision = i.Candidate.BaseRevision }},
		{"base", func(i *service.UnitReviewIdentity) { i.Candidate.BaseRevision = i.Candidate.Revision }},
		{"spec", func(i *service.UnitReviewIdentity) { i.Candidate.SpecRevision = "0" }},
		{"plan", func(i *service.UnitReviewIdentity) { i.Candidate.PlanRevision = "0" }},
	} {
		reviewed := identity
		tc.change(&reviewed)
		verdict := service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
		if tc.name == "plan" {
			verdict.Summary = ""
		}
		result := service.UnitReviewResult{Identity: reviewed, Turn: service.TestReviewTurnID("resume", state.Version), Verdict: verdict}
		if err := r.applyReview(context.Background(), stream, "resume", state, result); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		current, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		if err != nil || current.Value != intake.UnitReviewing || current.Version != state.Version+1 {
			t.Fatalf("%s: stale state %+v: %v", tc.name, current, err)
		}
		transitions := workTransitions(t, repo, stream)
		if !strings.Contains(transitions[len(transitions)-1].Reason, "stale "+tc.name) {
			t.Fatalf("%s: reason: %s", tc.name, transitions[len(transitions)-1].Reason)
		}

		moveUnitNow(t, f, repo, stream, "resume", execution.UnitChecking)
		moveUnitNow(t, f, repo, stream, "resume", intake.UnitReviewing)
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
	masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Built")
	entered := make(chan struct{})
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return chief.Turn(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "interrupted-review", independentPlan)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
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
	th, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
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
			p := &intakefixture.Faults{}
			f, masons, chief := newAskingMasonFixture(t, 1, independentPlan, p)
			defer f.stop(t)
			masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Built")
			var reviews atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			f.engine.Mu.Lock()
			f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
					if reviews.Add(1) > 1 {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						if !strings.Contains(req.Prompt, intakefixture.RelayedRuling) {
							return nil, fmt.Errorf("missing ruling in fresh review")
						}
						body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
						if err != nil || !strings.Contains(body, `"recorded":true`) {
							return nil, fmt.Errorf("verdict %s: %v", body, err)
						}
						return &agent.Result{ClaudeID: "reviewed", ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
					}
					if err := intakefixture.Asks(p, "1")(ctx, req, turn, tools); err != nil {
						return nil, err
					}
					return &agent.Result{ClaudeID: "asked", ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
				}
				return chief.Turn(ctx, req, turn, tools)
			}
			f.engine.Mu.Unlock()
			f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				if !strings.Contains(req.Prompt, intakefixture.RelayedRuling) {
					return fmt.Errorf("missing ruling: %s", req.Prompt)
				}
				if !answerVerdict {
					return nil
				}
				body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("verdict %s: %v", body, err)
				}
				return nil
			})
			stream := f.seedBuilding(t, "review-question", independentPlan)
			f.awaitUnit(t, stream, "resume", intake.UnitWaiting)

			asking := f.thread(t, stream, intake.ReviewerAgent("resume")).Turns[0]
			f.awaitCompleted(t, stream, intake.ReviewerAgent("resume"), asking.Request.TurnID)
			docs, err := trace.Read[trace.Document](f.repository(), stream)
			fixturekit.Must(t, err)
			var before service.UnitReviewIdentity
			for _, d := range docs {
				if d.ID == service.TestReviewDocument("resume") {
					_ = json.Unmarshal([]byte(d.Content), &before)
				}
			}
			if before.Candidate.Revision == "" {
				t.Fatal("review identity was not recorded")
			}
			f.stop(t)
			f.start(t)
			f.awaitUnit(t, stream, "resume", intake.UnitWaiting)
			f.rule(t, "1")
			if !answerVerdict {
				select {
				case <-started:
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("no fresh review after answer")
				}
				state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				if state.Value != intake.UnitReviewing {
					t.Fatalf("answer alone moved unit to %s", state.Value)
				}
				close(release)
			}
			f.awaitUnit(t, stream, "resume", intake.UnitMerged)
			r := &reviewers{masons: newMasonController(f.s, f.repository())}
			result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: intake.UnitApproved})
			fixturekit.Must(t, err)
			if !ok || result.Identity.Candidate.Revision != before.Candidate.Revision || (answerVerdict && result.Turn != questions.TurnID("1")) {
				t.Fatalf("review after answer: %+v", result)
			}
			th := f.thread(t, stream, intake.ReviewerAgent("resume"))
			wantTurns := 2
			if !answerVerdict {
				wantTurns++
			}
			if len(th.Turns) != wantTurns || th.Turns[0].Status() != questions.Waiting {
				t.Fatalf("reviewer turns: %+v", th.Turns)
			}
			p.Check(t)
			masons.Check(t)
		})
	}
}

func TestContestedReviewRulingSurvivesRestart(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	f.s.cfg.Shed.MaxBounces = 1
	masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Built")
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "material_findings", "summary": reviewSummary, "findings": []api.ReviewFinding{{Severity: "material", Evidence: "Retry fails", Action: "Handle retry"}}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "reviewed", ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.Turn(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "contested", independentPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitContested)
	r := &reviewers{masons: newMasonController(f.s, f.repository())}
	result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: intake.UnitContested})
	fixturekit.Must(t, err)
	if !ok || result.Bounces != 1 {
		t.Fatalf("bounce count: %+v", result)
	}
	raiseFixtureContest(t, f.repository(), stream, "resume")
	status, err := f.c.Status(context.Background(), stream)
	if err != nil || !slices.Contains(status.Gates, trace.OwnerGate{Kind: intake.UnitContested, Reference: "resume"}) {
		t.Fatalf("contested gate: %+v %v", status.Gates, err)
	}
	if entries := f.raisedContests(t, stream); len(entries) != 1 || entries[0].Unit != "resume" || !slices.Equal(entries[0].Options, []string{"review", "revise"}) ||
		entries[0].Answer.Path != "/v1/contested/"+string(stream)+"/resume" {
		t.Fatalf("inbox entries of the contested unit %+v", entries)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Check the candidate once more"); err != nil {
		t.Fatal(err)
	}

	if entries := f.inboxEntries(t, stream, api.InboxContested); len(entries) != 0 {
		t.Fatalf("inbox entries after the ruling %+v", entries)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Decide twice"); err == nil {
		t.Fatal("accepted duplicate ruling")
	}
	intakefixture.Soon(t, "the second review to contest the unit", func() bool {
		result, ok, err = r.storedResult(stream, "resume", trace.WorkflowState{Value: intake.UnitContested})
		fixturekit.Must(t, err)
		state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
		fixturekit.Must(t, err)
		return ok && result.Bounces == 2 && state.Value == intake.UnitContested
	})
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "The mason should fix the retry proof"); err != nil {
		t.Fatal(err)
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", intake.UnitImplementing)
	var th trace.Thread
	intakefixture.Soon(t, "the revision turn to be queued", func() bool {
		th = f.thread(t, stream, intake.MasonAgent("resume"))
		return len(th.Turns) == 2
	})
	if len(th.Turns) != 2 || (!strings.Contains(th.Turns[1].Request.Prompt, "Handle retry") || !strings.Contains(th.Turns[1].Request.Prompt, "The mason should fix the retry proof")) {
		t.Fatalf("revision turn: %+v", th.Turns)
	}
	if _, found, err := execution.LatestContestedRuling(f.repository(), stream, "resume", 2); err != nil || !found {
		t.Fatalf("ruling lost: %v %v", found, err)
	}
	masons.Check(t)
}

func TestReviewWaitsForTheRebaseOfAUnitLeftBehind(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, "review-behind", independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	seedReview(t, f, repository, stream, "dedupe", dedupeReport)
	approveDirectly(t, f.s, repository, stream, "resume", "spec#1")
	lands := &foreman{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	fixturekit.Must(t, lands.Pass(ctx))
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
	fixturekit.Must(t, err)
	if state.Value != intake.UnitReviewing {
		t.Fatalf("dedupe is %s, not reviewing", state.Value)
	}
	for range 2 {
		fixturekit.Must(t, r.one(ctx, stream, "dedupe", state, false))
	}
	if th, err := repository.Thread(stream, intake.ReviewerAgent("dedupe")); err == nil && len(th.Turns) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the candidate left behind was offered for review: %+v %v", th.Turns, err)
	}
	if after, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || after != state {
		t.Fatalf("dedupe moved from %+v to %+v while it waits for its rebase: %v", state, after, err)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	rebases := rebaseOperations(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("dedupe's rebases %+v", rebases)
	}
	settleOperation(t, f.s, repository, stream, rebases[0].Operation, rebaser{lands})
	if behind, err := units.behind(ctx, stream, "dedupe"); err != nil || behind {
		t.Fatalf("dedupe is behind after its rebase: %v %v", behind, err)
	}
}

func TestRepeatedlyRefusedReviewsContestTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "refresh-limit")
	r := &reviewers{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	for i := 1; i < execution.MaxReviewRefreshes; i++ {
		state, err := repository.Workflow(stream, subject)
		fixturekit.Must(t, err)
		fixturekit.Must(t, r.refreshReview(ctx, stream, "resume", state, "stale base revision; review the current candidate again"))
		if after, err := repository.Workflow(stream, subject); err != nil || after.Value != intake.UnitReviewing {
			t.Fatalf("refused review %d left the unit %+v: %v", i, after, err)
		}
	}
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.refreshReview(ctx, stream, "resume", state, "stale base revision; review the current candidate again"))
	contested, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	if contested.Value != intake.UnitContested {
		t.Fatalf("refused review %d left the unit %s", execution.MaxReviewRefreshes, contested.Value)
	}
	transitions := intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream)
	contest := transitions[len(transitions)-1]
	if contest.Subject != subject || !execution.FailedReview(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("refused %d reviews of unit resume in a row", execution.MaxReviewRefreshes)) {
		t.Fatalf("the contest %+v", contest)
	}
	if opts := service.TestContestOptions(contest, "resume", false); !slices.Equal(opts, []string{"review"}) {
		t.Fatalf("the contest offers %v", opts)
	}
}

func interruptTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	token := "stopped-" + th.Turns[len(th.Turns)-1].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, t.TempDir(), f.clock.Now())
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Cancelled: true}}
	fixturekit.Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.clock.Now()))
	return q
}

func TestRepeatedlyInterruptedReviewContestsTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "interrupted-review")
	r := &reviewers{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	for i := 1; i <= execution.MaxRecoveries; i++ {
		interruptTurn(t, f, repository, stream, intake.ReviewerAgent("resume"))
		fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
		got, err := repository.Workflow(stream, subject)
		fixturekit.Must(t, err)
		if i < execution.MaxRecoveries && got.Value != intake.UnitReviewing || i == execution.MaxRecoveries && got.Value != intake.UnitContested {
			t.Fatalf("after %d interruptions the unit is %s", i, got.Value)
		}
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.ReviewerAgent("resume")); len(ids) != execution.MaxRecoveries {
		t.Fatalf("reviewer turns %v", ids)
	}
	transitions := intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream)
	contest := transitions[len(transitions)-1]
	if !execution.FailedReview(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("the latest %d reviewer turns of unit resume were interrupted in a row", execution.MaxRecoveries)) {
		t.Fatalf("the contest %+v", contest)
	}

	if _, apiErr := f.s.moveUnit(ctx, repository, stream, "resume", api.UnitMoveRequest{To: intake.UnitReviewing, Note: "The service is stable again; review it."}, intake.ChiefActor, ""); apiErr != nil {
		t.Fatalf("the chief's move: %+v", apiErr)
	}
	state, err = repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	interruptTurn(t, f, repository, stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	if got, err := repository.Workflow(stream, subject); err != nil || got.Value != intake.UnitReviewing {
		t.Fatalf("after the move and one interruption the unit is %+v: %v", got, err)
	}
	ids := intakefixture.TurnIDs(t, repository, stream, intake.ReviewerAgent("resume"))
	if last := ids[len(ids)-1]; !strings.Contains(last, "-recover-") {
		t.Fatalf("the interrupted review after the move was not continued: %v", ids)
	}
}
