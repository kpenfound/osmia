package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRefusedVerdictReviewsTheCandidateAgain(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "refused-verdict")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	ctx := context.Background()
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
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
	journey.CaptureTurn(t, f, repo, stream, intake.ReviewerAgent("resume"), &coreadapter.Outcome{Status: execution.VerdictOutcome, Report: string(bad)})

	for range 2 {
		fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	}
	refreshed, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if refreshed.Value != intake.UnitReviewing || refreshed.Version != state.Version+1 {
		t.Fatalf("refused verdict left %+v, want reviewing at version %d", refreshed, state.Version+1)
	}
	if _, ok, err := r.Driver().StoredResult(stream, "resume", refreshed); err != nil || ok {
		t.Fatalf("refused verdict was stored: %v", err)
	}
	fixturekit.Must(t, r.One(ctx, stream, "resume", refreshed, false))
	fixturekit.Must(t, r.One(ctx, stream, "resume", refreshed, false))
	th, err = repo.Thread(stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != execution.ReviewTurnID("resume", refreshed.Version) {
		t.Fatalf("review turns after refusal: %+v", th.Turns)
	}
	if prompt := th.Turns[1].Request.Prompt; !strings.Contains(prompt, "was refused: summary is required: say how you verified the unit's acceptance") {
		t.Fatalf("fresh review lacks the refusal: %s", prompt)
	}
}

func TestReviewResultReconcilesAfterRestart(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "review-recovery")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := r.PrepareUnitReview(context.Background(), stream, "resume")
	if err != nil {
		t.Fatal(err)
	}
	turn := execution.ReviewTurnID("resume", state.Version)
	result := service.UnitReviewResult{Identity: identity, Turn: turn, Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}}
	data, _ := json.Marshal(result)
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == execution.ReviewDocument("resume") {
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
	repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	r = &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	if err := r.One(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.One(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	approved, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil || approved.Value != intake.UnitApproved {
		t.Fatalf("recovered %+v %v", approved, err)
	}
}

func TestStaleCandidateReturnsUnitToReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "stale-review")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := r.PrepareUnitReview(context.Background(), stream, "resume")
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
		verdict := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
		if tc.name == "plan" {
			verdict.Summary = ""
		}
		result := service.UnitReviewResult{Identity: reviewed, Turn: execution.ReviewTurnID("resume", state.Version), Verdict: verdict}
		if err := r.Driver().ApplyReview(context.Background(), stream, "resume", state, result); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		current, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		if err != nil || current.Value != intake.UnitReviewing || current.Version != state.Version+1 {
			t.Fatalf("%s: stale state %+v: %v", tc.name, current, err)
		}
		transitions := intakefixture.WorkTransitions(t, repo, stream)
		if !strings.Contains(transitions[len(transitions)-1].Reason, "stale "+tc.name) {
			t.Fatalf("%s: reason: %s", tc.name, transitions[len(transitions)-1].Reason)
		}

		intakefixture.MoveUnitNow(t, f.Clock.Now(), execution.ForemanActor, repo, stream, "resume", execution.UnitChecking)
		intakefixture.MoveUnitNow(t, f.Clock.Now(), execution.ForemanActor, repo, stream, "resume", intake.UnitReviewing)
		if state, err = repo.Workflow(stream, trace.UnitSubject("resume")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInterruptedReviewQueuesOneContinuation(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	stopped := false
	defer func() {
		if !stopped {
			f.Stop(t)
		}
	}()
	masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Built")
	entered := make(chan struct{})
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return chief.Turn(ctx, req, turn, tools)
	}
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "interrupted-review", intakefixture.IndependentPlan)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("review turn did not start")
	}
	f.Stop(t)
	stopped = true
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.One(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.One(context.Background(), stream, "resume", state, false); err != nil {
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
			f, masons, chief := newAskingMasonFixture(t, 1, intakefixture.IndependentPlan, p)
			defer f.Stop(t)
			masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Built")
			var reviews atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			f.Engine.Mu.Lock()
			f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
						body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
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
			f.Engine.Mu.Unlock()
			f.Answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				if !strings.Contains(req.Prompt, intakefixture.RelayedRuling) {
					return fmt.Errorf("missing ruling: %s", req.Prompt)
				}
				if !answerVerdict {
					return nil
				}
				body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("verdict %s: %v", body, err)
				}
				return nil
			})
			stream := seedBuilding(f, t, "review-question", intakefixture.IndependentPlan)
			f.AwaitUnit(t, stream, "resume", intake.UnitWaiting)

			asking := f.Thread(t, stream, intake.ReviewerAgent("resume")).Turns[0]
			awaitCompleted(f, t, stream, intake.ReviewerAgent("resume"), asking.Request.TurnID)
			docs, err := trace.Read[trace.Document](f.Repository(), stream)
			fixturekit.Must(t, err)
			var before service.UnitReviewIdentity
			for _, d := range docs {
				if d.ID == execution.ReviewDocument("resume") {
					_ = json.Unmarshal([]byte(d.Content), &before)
				}
			}
			if before.Candidate.Revision == "" {
				t.Fatal("review identity was not recorded")
			}
			f.Stop(t)
			f.Start(t)
			f.AwaitUnit(t, stream, "resume", intake.UnitWaiting)
			f.Rule(t, "1")
			if !answerVerdict {
				select {
				case <-started:
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("no fresh review after answer")
				}
				state, err := f.Repository().Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				if state.Value != intake.UnitReviewing {
					t.Fatalf("answer alone moved unit to %s", state.Value)
				}
				close(release)
			}
			f.AwaitUnit(t, stream, "resume", intake.UnitMerged)
			r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, f.Repository())}
			result, ok, err := r.Driver().StoredResult(stream, "resume", (trace.WorkflowState{Value: intake.UnitApproved}))
			fixturekit.Must(t, err)
			if !ok || result.Identity.Candidate.Revision != before.Candidate.Revision || (answerVerdict && result.Turn != questions.TurnID("1")) {
				t.Fatalf("review after answer: %+v", result)
			}
			th := f.Thread(t, stream, intake.ReviewerAgent("resume"))
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
	f, masons := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	defer f.Stop(t)
	f.S.Cfg.Shed.MaxBounces = 1
	masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Built")
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "material_findings", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{{Severity: "material", Evidence: "Retry fails", Action: "Handle retry"}}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "reviewed", ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.Turn(ctx, req, turn, tools)
	}
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "contested", intakefixture.IndependentPlan)
	f.AwaitUnit(t, stream, "resume", intake.UnitContested)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, f.Repository())}
	result, ok, err := r.Driver().StoredResult(stream, "resume", (trace.WorkflowState{Value: intake.UnitContested}))
	fixturekit.Must(t, err)
	if !ok || result.Bounces != 1 {
		t.Fatalf("bounce count: %+v", result)
	}
	journey.RaiseFixtureContest(t, f.Repository(), stream, "resume")
	status, err := f.C.Status(context.Background(), stream)
	if err != nil || !slices.Contains(status.Gates, trace.OwnerGate{Kind: intake.UnitContested, Reference: "resume"}) {
		t.Fatalf("contested gate: %+v %v", status.Gates, err)
	}
	if entries := raisedContests(f, t, stream); len(entries) != 1 || entries[0].Unit != "resume" || !slices.Equal(entries[0].Options, []string{"review", "revise"}) ||
		entries[0].Answer.Path != "/v1/contested/"+string(stream)+"/resume" {
		t.Fatalf("inbox entries of the contested unit %+v", entries)
	}
	if _, err := f.C.RuleContested(context.Background(), stream, "resume", "review", "Check the candidate once more"); err != nil {
		t.Fatal(err)
	}

	if entries := f.InboxEntries(t, stream, api.InboxContested); len(entries) != 0 {
		t.Fatalf("inbox entries after the ruling %+v", entries)
	}
	if _, err := f.C.RuleContested(context.Background(), stream, "resume", "review", "Decide twice"); err == nil {
		t.Fatal("accepted duplicate ruling")
	}
	intakefixture.Soon(t, "the second review to contest the unit", func() bool {
		result, ok, err = r.Driver().StoredResult(stream, "resume", (trace.WorkflowState{Value: intake.UnitContested}))
		fixturekit.Must(t, err)
		state, err := f.Repository().Workflow(stream, trace.UnitSubject("resume"))
		fixturekit.Must(t, err)
		return ok && result.Bounces == 2 && state.Value == intake.UnitContested
	})
	if _, err := f.C.RuleContested(context.Background(), stream, "resume", "revise", "The mason should fix the retry proof"); err != nil {
		t.Fatal(err)
	}
	f.Stop(t)
	f.Start(t)
	f.AwaitUnit(t, stream, "resume", intake.UnitImplementing)
	var th trace.Thread
	intakefixture.Soon(t, "the revision turn to be queued", func() bool {
		th = f.Thread(t, stream, intake.MasonAgent("resume"))
		return len(th.Turns) == 2
	})
	if len(th.Turns) != 2 || (!strings.Contains(th.Turns[1].Request.Prompt, "Handle retry") || !strings.Contains(th.Turns[1].Request.Prompt, "The mason should fix the retry proof")) {
		t.Fatalf("revision turn: %+v", th.Turns)
	}
	if _, found, err := execution.LatestContestedRuling(f.Repository(), stream, "resume", 2); err != nil || !found {
		t.Fatalf("ruling lost: %v %v", found, err)
	}
	masons.Check(t)
}

func TestReviewWaitsForTheRebaseOfAUnitLeftBehind(t *testing.T) {
	t.Parallel()
	f, _ := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := journey.SeedBuild(t, f, "review-behind", intakefixture.IndependentPlan, config.WorkspacesGit, base)
	journey.SeedReview(t, f, repository, stream, "resume", journey.ResumeReport)
	journey.SeedReview(t, f, repository, stream, "dedupe", dedupeReport)
	journey.ApproveDirectly(t, f.S, repository, stream, "resume", "spec#1")
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	ctx := context.Background()
	fixturekit.Must(t, lands.Pass(ctx))
	landings := journey.LandOperations(t, repository, stream)
	if len(landings) != 1 {
		t.Fatalf("landing operations %+v", landings)
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, landings[0].Operation, lands); result.Outcome != "succeeded" {
		t.Fatalf("landing result %+v", result)
	}
	units := journey.NewUnitWorkspaces(f.S.Cfg, repository)
	if behind, err := units.Driver().Behind(ctx, stream, "dedupe"); err != nil || !behind {
		t.Fatalf("dedupe is not behind the landed feature branch: %v %v", behind, err)
	}

	r := &journey.Reviewers{Masons: lands.Masons}
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitReviewing {
		t.Fatalf("dedupe is %s, not reviewing", state.Value)
	}
	for range 2 {
		fixturekit.Must(t, r.One(ctx, stream, "dedupe", state, false))
	}
	if th, err := repository.Thread(stream, intake.ReviewerAgent("dedupe")); err == nil && len(th.Turns) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the candidate left behind was offered for review: %+v %v", th.Turns, err)
	}
	if after, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || after != state {
		t.Fatalf("dedupe moved from %+v to %+v while it waits for its rebase: %v", state, after, err)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	rebases := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("dedupe's rebases %+v", rebases)
	}
	journey.SettleOperation(t, f.S, repository, stream, rebases[0].Operation, journey.Rebaser{Foreman: lands})
	if behind, err := units.Driver().Behind(ctx, stream, "dedupe"); err != nil || behind {
		t.Fatalf("dedupe is behind after its rebase: %v %v", behind, err)
	}
}

func TestRepeatedlyRefusedReviewsContestTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewReviewFixture(t, "refresh-limit")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	for i := 1; i < execution.MaxReviewRefreshes; i++ {
		state, err := repository.Workflow(stream, subject)
		fixturekit.Must(t, err)
		fixturekit.Must(t, r.Driver().RefreshReview(ctx, stream, "resume", state, ("stale base revision; review the current candidate again")))
		if after, err := repository.Workflow(stream, subject); err != nil || after.Value != intake.UnitReviewing {
			t.Fatalf("refused review %d left the unit %+v: %v", i, after, err)
		}
	}
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.Driver().RefreshReview(ctx, stream, "resume", state, ("stale base revision; review the current candidate again")))
	contested, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	if contested.Value != intake.UnitContested {
		t.Fatalf("refused review %d left the unit %s", execution.MaxReviewRefreshes, contested.Value)
	}
	transitions := intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream)
	contest := transitions[len(transitions)-1]
	if contest.Subject != subject || !execution.FailedReview(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("refused %d reviews of unit resume in a row", execution.MaxReviewRefreshes)) {
		t.Fatalf("the contest %+v", contest)
	}
	if opts := service.ContestOptions(contest, "resume", false); !slices.Equal(opts, []string{"review"}) {
		t.Fatalf("the contest offers %v", opts)
	}
}

func interruptTurn(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	token := "stopped-" + th.Turns[len(th.Turns)-1].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, t.TempDir(), f.Clock.Now())
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.Clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Cancelled: true}}
	fixturekit.Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.Clock.Now()))
	return q
}

func TestRepeatedlyInterruptedReviewContestsTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewReviewFixture(t, "interrupted-review")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	for i := 1; i <= execution.MaxRecoveries; i++ {
		interruptTurn(t, f, repository, stream, intake.ReviewerAgent("resume"))
		fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
		got, err := repository.Workflow(stream, subject)
		fixturekit.Must(t, err)
		if i < execution.MaxRecoveries && got.Value != intake.UnitReviewing || i == execution.MaxRecoveries && got.Value != intake.UnitContested {
			t.Fatalf("after %d interruptions the unit is %s", i, got.Value)
		}
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.ReviewerAgent("resume")); len(ids) != execution.MaxRecoveries {
		t.Fatalf("reviewer turns %v", ids)
	}
	transitions := intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream)
	contest := transitions[len(transitions)-1]
	if !execution.FailedReview(contest, "resume") || !strings.Contains(contest.Reason, fmt.Sprintf("the latest %d reviewer turns of unit resume were interrupted in a row", execution.MaxRecoveries)) {
		t.Fatalf("the contest %+v", contest)
	}

	if _, apiErr := f.S.Execution().MoveUnit(ctx, repository, stream, "resume", (api.UnitMoveRequest{To: intake.UnitReviewing, Note: "The service is stable again; review it."}), intake.ChiefActor, ""); apiErr != nil {
		t.Fatalf("the chief's move: %+v", apiErr)
	}
	state, err = repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	interruptTurn(t, f, repository, stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	if got, err := repository.Workflow(stream, subject); err != nil || got.Value != intake.UnitReviewing {
		t.Fatalf("after the move and one interruption the unit is %+v: %v", got, err)
	}
	ids := intakefixture.TurnIDs(t, repository, stream, intake.ReviewerAgent("resume"))
	if last := ids[len(ids)-1]; !strings.Contains(last, "-recover-") {
		t.Fatalf("the interrupted review after the move was not continued: %v", ids)
	}
}
