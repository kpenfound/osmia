package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// holdQuestionReview lets delivery and dispatch run while the reviewer
// controller is delayed. It changes no durable state or controller outcome.
func holdQuestionReview(f *shedFixture, stream config.WorkstreamID, reviews, dispatch *atomic.Bool) {
	p := f.s.sole()
	st := *p.pipeline.current.Load()
	st.hooks = slices.Clone(st.hooks)
	for i, hook := range st.hooks {
		if hook.name != "reviews" && hook.name != "dispatch" {
			continue
		}
		st.hooks[i].pass = func(ctx context.Context) error {
			state, err := p.repository.Workflow(stream, trace.UnitSubject("resume"))
			if err != nil {
				return err
			}
			if state.Value == UnitWaiting {
				if hook.name == "reviews" && reviews.Load() {
					return nil
				}
				if hook.name == "dispatch" && dispatch.Load() {
					asked, err := p.repository.Questions(stream)
					if err != nil {
						return err
					}
					th, err := p.repository.Thread(stream, reviewerAgent("resume"))
					if err != nil {
						return err
					}
					if len(asked) == 1 && questionDelivered(asked[0], th) {
						return nil
					}
				}
			}
			return hook.pass(ctx)
		}
	}
	p.pipeline.next.Store(&st)
}

func awaitQuestionJourney(t *testing.T, f *shedFixture, phase string, ready func() bool) {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(demoTimeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out awaiting %s", phase)
		}
		wait()
	}
}

func stopQuestionJourney(t *testing.T, f *shedFixture) {
	t.Helper()
	f.c.Close()
	s, stopped := f.s, f.serviceDone
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("question journey service did not shut down")
	}
	select {
	case <-stopped:
	case <-time.After(demoTimeout):
		t.Fatal("question journey service waiter did not finish")
	}
}

func questionJourneyTurn(t *testing.T, f *shedFixture, stream config.WorkstreamID, agent string, index int) trace.QueuedTurn {
	t.Helper()
	var turn trace.QueuedTurn
	awaitQuestionJourney(t, f, fmt.Sprintf("%s turn %d completion", agent, index), func() bool {
		th, err := f.repository().Thread(stream, agent)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		must(t, err)
		if len(th.Turns) <= index {
			return false
		}
		turn = th.Turns[index]
		return !turn.CompletedAt.IsZero()
	})
	return turn
}

func awaitQuestionJourneyQuiet(t *testing.T, f *shedFixture, stream config.WorkstreamID) {
	t.Helper()
	awaitQuestionJourney(t, f, "chief notices and turn completion", func() bool {
		outbox, err := f.repository().Outbox(stream)
		must(t, err)
		for _, entry := range outbox {
			if chiefEvent(entry.Event) && !entry.Acknowledged {
				return false
			}
		}
		chief, err := f.repository().ChiefOfStaffThread(stream)
		must(t, err)
		return !slices.ContainsFunc(chief.Turns, func(turn trace.QueuedTurn) bool { return turn.CompletedAt.IsZero() })
	})
}

// The running service owns every transition, answer delivery and landing.
// Only model turns and the project's check executor are fake.
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
			problems := &faults{}
			passed, prepare := countingSchedule()
			f, masons, chief := newAskingMasonFixturePrepared(t, 1, validPlan, problems, func(opts *Options) {
				prepare(opts)
				opts.ShutdownTimeout = 10 * time.Second
			})
			defer func() { stopQuestionJourney(t, f) }()
			masons.play[masonTurnID("resume")] = reportDone("Resume uploads")
			masons.play[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n"), 0600); err != nil {
					return err
				}
				return reportDone("Skip acknowledged chunks")(ctx, req, tools)
			}
			verdict := func(ctx context.Context, tools *mcp.ClientSession) error {
				body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("verdict: %s: %v", body, err)
				}
				return nil
			}
			identities := make(chan UnitReviewIdentity, 1)
			buildReady := make(chan struct{})
			var reviewCalls, answerCalls, finalCalls atomic.Int32
			var stream config.WorkstreamID
			f.engine.mu.Lock()
			f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				switch {
				case strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-"):
					if reviewCalls.Add(1) != 1 {
						return nil, fmt.Errorf("duplicate review of resume: %s", req.Name)
					}
					identity, err := reviewIdentityInPrompt(req.Prompt)
					if err != nil {
						return nil, err
					}
					identities <- identity
					if err := asks(problems, "1")(ctx, req, turn, tools); err != nil {
						return nil, err
					}
				case strings.HasPrefix(req.Name, reviewerAgent("dedupe")+"-review-"):
					if err := verdict(ctx, tools); err != nil {
						return nil, err
					}
				default:
					return chief.turn(ctx, req, turn, tools)
				}
				return questionResult(req, "session-"+req.Name, "Reviewed"), nil
			}
			f.engine.mu.Unlock()
			f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				<-buildReady
				answerCalls.Add(1)
				if !strings.Contains(req.Prompt, ownerRuling) || !strings.Contains(req.Prompt, relayedRuling) {
					return fmt.Errorf("reviewer did not receive the owner's ruling: %s", req.Prompt)
				}
				state, err := f.unitState(stream, "resume")
				if err != nil || (scenario.waiting && !scenario.queuedOnly && state != UnitWaiting) {
					return fmt.Errorf("answer turn state %s: %v", state, err)
				}
				return verdict(ctx, tools)
			})
			finalTurn := f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				<-buildReady
				finalCalls.Add(1)
				for _, unit := range []string{"resume", "dedupe"} {
					state, err := f.unitState(stream, unit)
					if err != nil || state != UnitMerged {
						return fmt.Errorf("final review started before %s merged: %s: %v", unit, state, err)
					}
				}
				body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
				if err != nil || !strings.Contains(body, `"recorded":true`) {
					return fmt.Errorf("final report: %s: %v", body, err)
				}
				return nil
			})

			factory := runtime.Target{Scope: "factory"}
			stream = f.builtPaused(t, factory, scenario.name)[0]
			close(buildReady)
			var holdReviews, holdDispatch atomic.Bool
			holdReviews.Store(scenario.waiting)
			holdDispatch.Store(scenario.queuedOnly)
			holdQuestionReview(f, stream, &holdReviews, &holdDispatch)
			mutation(t, f.c, "DELETE", "pause", factory)
			awaitQuestionJourney(t, f, "reviewer wait and escalation", func() bool {
				state, err := f.unitState(stream, "resume")
				must(t, err)
				asked, err := f.repository().Questions(stream)
				must(t, err)
				return state == UnitWaiting && len(asked) == 1 && asked[0].State == trace.QuestionEscalated
			})
			asking := questionJourneyTurn(t, f, stream, reviewerAgent("resume"), 0)
			identity := <-identities
			awaitPasses(t, passed, 3)
			assertWaiting := func() {
				t.Helper()
				state, err := f.unitState(stream, "resume")
				must(t, err)
				if state != UnitWaiting || len(finalOperations(t, f.repository(), stream)) != 0 || finalCalls.Load() != 0 || len(landOperations(t, f.repository(), stream)) != 0 {
					t.Fatalf("waiting unit advanced: state %s, final calls %d", state, finalCalls.Load())
				}
			}
			assertWaiting()
			unanswered := f.question(t, stream, "1")
			if unanswered.Ruling != nil || len(f.thread(t, stream, reviewerAgent("resume")).Turns) != 1 {
				t.Fatalf("unanswered question resumed: %+v", unanswered)
			}
			before, err := trace.Read[trace.Transition](f.repository(), stream)
			must(t, err)
			f.rule(t, "1")
			if scenario.waiting {
				awaitQuestionJourney(t, f, "delivered owner ruling", func() bool {
					th := f.thread(t, stream, reviewerAgent("resume"))
					q := f.question(t, stream, "1")
					return q.State == trace.QuestionAnswered && questionDelivered(q, th)
				})
				if !scenario.queuedOnly {
					answer := questionJourneyTurn(t, f, stream, reviewerAgent("resume"), 1)
					if answer.Response == nil || answer.Response.Result.Outcome == nil || answer.Response.Result.Outcome.Status != verdictOutcome {
						t.Fatalf("answer turn lost its verdict: %+v", answer)
					}
				} else if th := f.thread(t, stream, reviewerAgent("resume")); len(th.Turns) != 2 || th.Turns[1].Claim != nil || !th.Turns[1].CompletedAt.IsZero() {
					t.Fatalf("delivered answer ran before restart: %+v", th.Turns)
				}
				awaitPasses(t, passed, 3)
				assertWaiting()
				before, err = trace.Read[trace.Transition](f.repository(), stream)
				must(t, err)
				if scenario.restart {
					awaitQuestionJourneyQuiet(t, f, stream)
					stopQuestionJourney(t, f)
					f.start(t)
				}
				holdReviews.Store(false)
				holdDispatch.Store(false)
			}

			for _, unit := range []string{"resume", "dedupe"} {
				f.awaitMerged(t, stream, unit)
			}
			questionJourneyTurn(t, f, stream, reviewerAgent("resume"), 1)
			questionJourneyTurn(t, f, stream, committeeAgent(1), 0)
			awaitQuestionJourney(t, f, "final review completion", func() bool {
				ops := finalOperations(t, f.repository(), stream)
				return len(ops) == 1 && ops[0].Acknowledged && ops[0].Result != nil && ops[0].Result.Outcome == "succeeded"
			})
			if th := f.thread(t, stream, committeeAgent(1)); len(th.Turns) != 1 || th.Turns[0].Request.TurnID != finalTurn {
				t.Fatalf("final reviewer turns: %+v", th.Turns)
			}
			_, approved := approvedReview(t, f.repository(), stream, "resume")
			if approved.Identity != identity || approved.Turn != questions.TurnID("1") || approved.Verdict.Decision != "satisfactory" {
				t.Fatalf("approval lost the unchanged candidate or answer verdict: %+v, asked %+v", approved, identity)
			}
			q := f.question(t, stream, "1")
			if !reflect.DeepEqual(q.Asked, unanswered.Asked) || !reflect.DeepEqual(f.thread(t, stream, reviewerAgent("resume")).Turns[0], asking) {
				t.Fatal("question recovery changed the original question or asking turn")
			}
			if q.Ruling == nil || q.Ruling.Decision != trace.DecisionRuling || q.Ruling.OwnerResponse != ownerRuling || q.Ruling.ReturnedAnswer != relayedRuling {
				t.Fatalf("owner ruling history: %+v", q)
			}
			assertQuestionJourneyHistory(t, f, stream, before)
			// Repeated live passes and another restart must keep the completed
			// path, including notices and exact worker turns, unchanged.
			awaitPasses(t, passed, 3)
			awaitQuestionJourneyQuiet(t, f, stream)
			snapshot := questionJourneySnapshot(t, f, stream)
			stopQuestionJourney(t, f)
			f.start(t)
			awaitPasses(t, passed, 3)
			awaitQuestionJourneyQuiet(t, f, stream)
			if after := questionJourneySnapshot(t, f, stream); !reflect.DeepEqual(snapshot, after) {
				t.Fatal("restart duplicated or changed transitions, turns, notices or landings")
			}
			if reviewCalls.Load() != 1 || answerCalls.Load() != 1 || finalCalls.Load() != 1 {
				t.Fatalf("model turns: review %d, answer %d, final %d", reviewCalls.Load(), answerCalls.Load(), finalCalls.Load())
			}
			masons.check(t)
			problems.check(t)
		})
	}
}

func assertQuestionJourneyHistory(t *testing.T, f *shedFixture, stream config.WorkstreamID, before []trace.Transition) {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.repository(), stream)
	must(t, err)
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
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
		want := []string{UnitPlanned, UnitReady, UnitImplementing, UnitChecking, UnitReviewing, UnitApproved, UnitMerged}
		if unit == "resume" {
			want = slices.Insert(want, 5, UnitWaiting, UnitReviewing)
		}
		if !slices.Equal(path, want) {
			t.Fatalf("%s transitions %v, want %v", unit, path, want)
		}
		th := f.thread(t, stream, reviewerAgent(unit))
		wantTurns := 1
		if unit == "resume" {
			wantTurns = 2
		}
		if len(th.Turns) != wantTurns {
			t.Fatalf("%s reviewer turns: %+v", unit, th.Turns)
		}
		mason := f.thread(t, stream, masonAgent(unit))
		if len(mason.Turns) != 1 || mason.Turns[0].CompletedAt.IsZero() {
			t.Fatalf("%s mason turns: %+v", unit, mason.Turns)
		}
		if docs := streamDocuments(t, f.repository(), stream, landingDocument(unit)); len(docs) != 1 {
			t.Fatalf("%s landings: %+v", unit, docs)
		}
		for _, prefix := range []string{"Unit " + unit + " review satisfactory:", "Unit " + unit + " merged:"} {
			count := 0
			for _, entry := range outbox {
				if chiefEvent(entry.Event) && strings.HasPrefix(entry.Event.Body, prefix) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%q notices: %d, want one", prefix, count)
			}
		}
	}
	if ops := landOperations(t, f.repository(), stream); len(ops) != 2 {
		t.Fatalf("landing operations: %+v", ops)
	}
}

// questionJourneySnapshot retains immutable evidence; acknowledgements and
// delivery claims may still settle independently of the unit controllers.
func questionJourneySnapshot(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.repository(), stream)
	must(t, err)
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	var notices []trace.Event
	for _, e := range outbox {
		if chiefEvent(e.Event) {
			notices = append(notices, e.Event)
		}
	}
	var threads []trace.Thread
	for _, agent := range []string{masonAgent("resume"), masonAgent("dedupe"), reviewerAgent("resume"), reviewerAgent("dedupe"), committeeAgent(1)} {
		threads = append(threads, f.thread(t, stream, agent))
	}
	chief, err := f.repository().ChiefOfStaffThread(stream)
	must(t, err)
	threads = append(threads, chief)
	data, err := json.Marshal([]any{transitions, docs, notices, threads})
	must(t, err)
	return string(data)
}
