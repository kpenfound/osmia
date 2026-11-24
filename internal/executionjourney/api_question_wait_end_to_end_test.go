package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	questions "github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func holdQuestionReview(f *journey.ShedFixture, stream config.WorkstreamID, reviews, dispatch *atomic.Bool) {
	p := f.S.Sole()
	st := *service.InspectForTest(f.S.Core()).Pipeline(p.Id).Current.Load()
	st.Hooks = slices.Clone(st.Hooks)
	for i, hook := range st.Hooks {
		if hook.Name != "reviews" && hook.Name != "dispatch" {
			continue
		}
		st.Hooks[i].Pass = func(ctx context.Context) error {
			state, err := p.Repository.Workflow(stream, trace.UnitSubject("resume"))
			if err != nil {
				return err
			}
			if state.Value == intake.UnitWaiting {
				if hook.Name == "reviews" && reviews.Load() {
					return nil
				}
				if hook.Name == "dispatch" && dispatch.Load() {
					asked, err := p.Repository.Questions(stream)
					if err != nil {
						return err
					}
					th, err := p.Repository.Thread(stream, intake.ReviewerAgent("resume"))
					if err != nil {
						return err
					}
					if len(asked) == 1 && execution.QuestionDelivered(asked[0], th) {
						return nil
					}
				}
			}
			return hook.Pass(ctx)
		}
	}
	service.InspectForTest(f.S.Core()).Pipeline(p.Id).Next.Store(&st)
}

func awaitQuestionJourney(t *testing.T, f *journey.ShedFixture, phase string, ready func() bool) {
	t.Helper()
	wait, closeWait := journey.ServiceChanges(t, f.S)
	defer closeWait()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out awaiting %s", phase)
		}
		wait()
	}
}

func stopQuestionJourney(t *testing.T, f *journey.ShedFixture) {
	t.Helper()
	f.C.Close()
	s, stopped := f.S, f.ServiceDone
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("question journey service did not shut down")
	}
	select {
	case <-stopped:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("question journey service waiter did not finish")
	}
}

func questionJourneyTurn(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, agent string, index int) trace.QueuedTurn {
	t.Helper()
	var turn trace.QueuedTurn
	awaitQuestionJourney(t, f, fmt.Sprintf("%s turn %d completion", agent, index), func() bool {
		th, err := f.Repository().Thread(stream, agent)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		fixturekit.Must(t, err)
		if len(th.Turns) <= index {
			return false
		}
		turn = th.Turns[index]
		return !turn.CompletedAt.IsZero()
	})
	return turn
}

func awaitQuestionJourneyQuiet(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) {
	t.Helper()
	awaitQuestionJourney(t, f, "chief notices and turn completion", func() bool {
		outbox, err := f.Repository().Outbox(stream)
		fixturekit.Must(t, err)
		for _, entry := range outbox {
			if intakefixture.ChiefEvent(entry.Event) && !entry.Acknowledged {
				return false
			}
		}
		chief, err := f.Repository().ChiefOfStaffThread(stream)
		fixturekit.Must(t, err)
		return !slices.ContainsFunc(chief.Turns, func(turn trace.QueuedTurn) bool { return turn.CompletedAt.IsZero() })
	})
}

func TestReviewerQuestionOwnerRulingReachesFinalReview(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name                         string
		restart, waiting, queuedOnly bool
	}{
		{name: "answer-turn"},
		{name: "restart-with-delivered-answer", restart: true, waiting: true, queuedOnly: true},
		{name: "verdict-while-waiting", waiting: true},
		{name: "restart-with-completed-verdict", restart: true, waiting: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			problems := &intakefixture.Faults{}
			passed, prepare := journey.CountingSchedule()
			f, masons, chief := newAskingMasonFixturePrepared(t, 1, fixturekit.ValidPlan, problems, func(opts *journey.Options) {
				prepare(opts)
				opts.ShutdownTimeout = 10 * time.Second
			})
			defer func() { stopQuestionJourney(t, f) }()
			masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Resume uploads")
			masons.Play[execution.MasonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n"), 0600); err != nil {
					return err
				}
				return intakefixture.ReportDone("Skip acknowledged chunks")(ctx, req, tools)
			}
			verdict := func(ctx context.Context, tools *mcp.ClientSession) error {
				body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("verdict: %s: %v", body, err)
				}
				return nil
			}
			identities := make(chan service.UnitReviewIdentity, 1)
			buildReady := make(chan struct{})
			var reviewCalls, answerCalls, finalCalls atomic.Int32
			var stream config.WorkstreamID
			hostRunningService(t, f, func() config.WorkstreamID { return stream })
			f.Engine.Mu.Lock()
			f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				switch {
				case strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-"):
					if reviewCalls.Add(1) != 1 {
						return nil, fmt.Errorf("duplicate review of resume: %s", req.Name)
					}
					identity, err := reviewtools.IdentityInPrompt(req.Prompt)
					if err != nil {
						return nil, err
					}
					identities <- identity
					if err := intakefixture.Asks(problems, "1")(ctx, req, turn, tools); err != nil {
						return nil, err
					}
				case strings.HasPrefix(req.Name, intake.ReviewerAgent("dedupe")+"-review-"):
					if err := verdict(ctx, tools); err != nil {
						return nil, err
					}
				default:
					return chief.Turn(ctx, req, turn, tools)
				}
				return intakefixture.QuestionResult(req, "session-"+req.Name, "Reviewed"), nil
			}
			f.Engine.Mu.Unlock()
			f.Answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				<-buildReady
				answerCalls.Add(1)
				if !strings.Contains(req.Prompt, intakefixture.OwnerRuling) || !strings.Contains(req.Prompt, intakefixture.RelayedRuling) {
					return fmt.Errorf("reviewer did not receive the owner's ruling: %s", req.Prompt)
				}
				state, err := f.UnitState(stream, "resume")
				if err != nil || (scenario.waiting && !scenario.queuedOnly && state != intake.UnitWaiting) {
					return fmt.Errorf("answer turn state %s: %v", state, err)
				}
				return verdict(ctx, tools)
			})
			finalTurn := f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				<-buildReady
				finalCalls.Add(1)
				for _, unit := range []string{"resume", "dedupe"} {
					state, err := f.UnitState(stream, unit)
					if err != nil || state != intake.UnitMerged {
						return fmt.Errorf("final review started before %s merged: %s: %v", unit, state, err)
					}
				}
				body, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("final report: %s: %v", body, err)
				}
				return nil
			})

			factory := runtime.Target{Scope: "factory"}
			stream = builtPaused(f, t, factory, scenario.name)[0]
			close(buildReady)
			var holdReviews, holdDispatch atomic.Bool
			holdReviews.Store(scenario.waiting)
			holdDispatch.Store(scenario.queuedOnly)
			holdQuestionReview(f, stream, &holdReviews, &holdDispatch)
			intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
			awaitQuestionJourney(t, f, "reviewer wait and escalation", func() bool {
				state, err := f.UnitState(stream, "resume")
				fixturekit.Must(t, err)
				asked, err := f.Repository().Questions(stream)
				fixturekit.Must(t, err)
				return state == intake.UnitWaiting && len(asked) == 1 && asked[0].State == trace.QuestionEscalated
			})
			asking := questionJourneyTurn(t, f, stream, intake.ReviewerAgent("resume"), 0)
			identity := <-identities
			intakefixture.AwaitPasses(t, passed, 3)
			assertWaiting := func() {
				t.Helper()
				state, err := f.UnitState(stream, "resume")
				fixturekit.Must(t, err)
				if state != intake.UnitWaiting || len(journey.FinalOperations(t, f.Repository(), stream)) != 0 || finalCalls.Load() != 0 || len(journey.LandOperations(t, f.Repository(), stream)) != 0 {
					t.Fatalf("waiting unit advanced: state %s, final calls %d", state, finalCalls.Load())
				}
			}
			assertWaiting()
			unanswered := f.Question(t, stream, "1")
			if unanswered.Ruling != nil || len(f.Thread(t, stream, intake.ReviewerAgent("resume")).Turns) != 1 {
				t.Fatalf("unanswered question resumed: %+v", unanswered)
			}
			before, err := trace.Read[trace.Transition](f.Repository(), stream)
			fixturekit.Must(t, err)
			f.Rule(t, "1")
			if scenario.waiting {
				awaitQuestionJourney(t, f, "delivered owner ruling", func() bool {
					th := f.Thread(t, stream, intake.ReviewerAgent("resume"))
					q := f.Question(t, stream, "1")
					return q.State == trace.QuestionAnswered && execution.QuestionDelivered(q, th)
				})
				if !scenario.queuedOnly {
					answer := questionJourneyTurn(t, f, stream, intake.ReviewerAgent("resume"), 1)
					if answer.Response == nil || answer.Response.Result.Outcome == nil || answer.Response.Result.Outcome.Status != execution.VerdictOutcome {
						t.Fatalf("answer turn lost its verdict: %+v", answer)
					}
				} else if th := f.Thread(t, stream, intake.ReviewerAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Claim != nil || !th.Turns[1].CompletedAt.IsZero() {
					t.Fatalf("delivered answer ran before restart: %+v", th.Turns)
				}
				intakefixture.AwaitPasses(t, passed, 3)
				assertWaiting()
				before, err = trace.Read[trace.Transition](f.Repository(), stream)
				fixturekit.Must(t, err)
				if scenario.restart {
					awaitQuestionJourneyQuiet(t, f, stream)
					stopQuestionJourney(t, f)
					f.Start(t)
				}
				holdReviews.Store(false)
				holdDispatch.Store(false)
			}

			for _, unit := range []string{"resume", "dedupe"} {
				f.AwaitMerged(t, stream, unit)
			}
			questionJourneyTurn(t, f, stream, intake.ReviewerAgent("resume"), 1)
			questionJourneyTurn(t, f, stream, intake.CommitteeAgent(1), 0)
			awaitQuestionJourney(t, f, "final review completion", func() bool {
				ops := journey.FinalOperations(t, f.Repository(), stream)
				return len(ops) == 1 && ops[0].Acknowledged && ops[0].Result != nil && ops[0].Result.Outcome == "succeeded"
			})
			if th := f.Thread(t, stream, intake.CommitteeAgent(1)); len(th.Turns) != 1 || th.Turns[0].Request.TurnID != finalTurn {
				t.Fatalf("final reviewer turns: %+v", th.Turns)
			}
			_, approved := journey.ApprovedReview(t, f.Repository(), stream, "resume")
			if approved.Identity != identity || approved.Turn != questions.TurnID("1") || approved.Verdict.Decision != "satisfactory" {
				t.Fatalf("approval lost the unchanged candidate or answer verdict: %+v, asked %+v", approved, identity)
			}
			q := f.Question(t, stream, "1")
			if !reflect.DeepEqual(q.Asked, unanswered.Asked) || !reflect.DeepEqual(f.Thread(t, stream, intake.ReviewerAgent("resume")).Turns[0], asking) {
				t.Fatal("question recovery changed the original question or asking turn")
			}
			if q.Ruling == nil || q.Ruling.Decision != trace.DecisionRuling || q.Ruling.OwnerResponse != intakefixture.OwnerRuling || q.Ruling.ReturnedAnswer != intakefixture.RelayedRuling {
				t.Fatalf("owner ruling history: %+v", q)
			}
			assertQuestionJourneyHistory(t, f, stream, before)

			intakefixture.AwaitPasses(t, passed, 3)
			awaitQuestionJourneyQuiet(t, f, stream)
			snapshot := questionJourneySnapshot(t, f, stream)
			stopQuestionJourney(t, f)
			f.Start(t)
			intakefixture.AwaitPasses(t, passed, 3)
			awaitQuestionJourneyQuiet(t, f, stream)
			if after := questionJourneySnapshot(t, f, stream); !reflect.DeepEqual(snapshot, after) {
				t.Fatal("restart duplicated or changed transitions, turns, notices or landings")
			}
			if reviewCalls.Load() != 1 || answerCalls.Load() != 1 || finalCalls.Load() != 1 {
				t.Fatalf("model turns: review %d, answer %d, final %d", reviewCalls.Load(), answerCalls.Load(), finalCalls.Load())
			}
			masons.Check(t)
			problems.Check(t)
		})
	}
}

func assertQuestionJourneyHistory(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, before []trace.Transition) {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.Repository(), stream)
	fixturekit.Must(t, err)
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	if len(transitions) < len(before) || !reflect.DeepEqual(transitions[:len(before)], before) {
		t.Fatal("question recovery rewrote recorded history")
	}
	for _, unit := range []string{"resume", "dedupe"} {
		var path []string
		for _, tr := range transitions {
			if tr.Subject == trace.UnitSubject(unit) {
				path = append(path, tr.To)
			}
		}
		want := []string{intake.UnitPlanned, intake.UnitReady, intake.UnitImplementing, execution.UnitChecking, intake.UnitReviewing, intake.UnitApproved, intake.UnitMerged}
		if unit == "resume" {
			want = slices.Insert(want, 5, intake.UnitWaiting, intake.UnitReviewing)
		}
		if !slices.Equal(path, want) {
			t.Fatalf("%s transitions %v, want %v", unit, path, want)
		}
		th := f.Thread(t, stream, intake.ReviewerAgent(unit))
		wantTurns := 1
		if unit == "resume" {
			wantTurns = 2
		}
		if len(th.Turns) != wantTurns {
			t.Fatalf("%s reviewer turns: %+v", unit, th.Turns)
		}
		mason := f.Thread(t, stream, intake.MasonAgent(unit))
		if len(mason.Turns) != 1 || mason.Turns[0].CompletedAt.IsZero() {
			t.Fatalf("%s mason turns: %+v", unit, mason.Turns)
		}
		if docs := intakefixture.StreamDocuments(t, f.Repository(), stream, execution.LandingDocument(unit)); len(docs) != 1 {
			t.Fatalf("%s landings: %+v", unit, docs)
		}
		for _, prefix := range []string{"Unit " + unit + " review satisfactory:", "Unit " + unit + " merged:"} {
			count := 0
			for _, entry := range outbox {
				if intakefixture.ChiefEvent(entry.Event) && strings.HasPrefix(entry.Event.Body, prefix) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%q notices: %d, want one", prefix, count)
			}
		}
	}
	if ops := journey.LandOperations(t, f.Repository(), stream); len(ops) != 2 {
		t.Fatalf("landing operations: %+v", ops)
	}
}

func questionJourneySnapshot(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.Repository(), stream)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](f.Repository(), stream)
	fixturekit.Must(t, err)
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	var notices []trace.Event
	for _, e := range outbox {
		if intakefixture.ChiefEvent(e.Event) {
			notices = append(notices, e.Event)
		}
	}
	var threads []trace.Thread
	for _, agent := range []string{intake.MasonAgent("resume"), intake.MasonAgent("dedupe"), intake.ReviewerAgent("resume"), intake.ReviewerAgent("dedupe"), intake.CommitteeAgent(1)} {
		threads = append(threads, f.Thread(t, stream, agent))
	}
	chief, err := f.Repository().ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	threads = append(threads, chief)
	data, err := json.Marshal([]any{transitions, docs, notices, threads})
	fixturekit.Must(t, err)
	return string(data)
}
