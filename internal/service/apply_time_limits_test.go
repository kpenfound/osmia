package service

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func refusedNoticesOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []string {
	t.Helper()
	outbox, err := repo.Outbox(stream)
	fixturekit.Must(t, err)
	var out []string
	for _, e := range outbox {
		if strings.HasPrefix(e.TransitionID, "route-") && strings.HasSuffix(e.TransitionID, "-refused") {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

func refusedJudgmentsOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []jev.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "judgments/") && d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []jev.Record
	for _, d := range latest {
		var r jev.Record
		fixturekit.Must(t, json.Unmarshal([]byte(d.Content), &r))
		if r.Refused != "" {
			out = append(out, r)
		}
	}
	return out
}

func sendBackExit(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, cause string) (router.Exit, string) {
	t.Helper()
	id := "unit-resume-implementing-" + cause
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: execution.TriggerChecksFailed, Cause: cause,
		Default: UnitImplementing, Targets: []string{UnitImplementing, UnitChecking, UnitReviewing, UnitContested}, Actor: foremanActor,
		ID: id, HeaderCause: cause, Reason: "check run 1 failed", Note: "check run 1 failed",
		Events: []trace.Event{trace.Notice(id, "unit", "Unit resume checks failed: check run 1 failed.")}})
	fixturekit.Must(t, err)
	return x, id
}

func unitStateOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) trace.WorkflowState {
	t.Helper()
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	return state
}

type exhaustion struct {
	arm    func(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID)
	reach  func(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) error
	reason string
}

var exhausted = map[string]exhaustion{
	"the daily budget": {
		arm: func(*testing.T, *shedFixture, *trace.Repository, config.WorkstreamID) {},
		reach: func(_ *testing.T, f *shedFixture, _ *trace.Repository, stream config.WorkstreamID) error {
			return f.s.store.SetPause(runtime.Pause{Target: workstreamTarget(f, stream), Mode: "soft", Reason: "the daily budget is spent", Source: runtime.PauseDailyBudget, SetAt: f.clock.Now()})
		},
		reason: "the daily budget is spent",
	},
	"the loop guard": {
		arm: func(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) {
			f.s.cfg.Loop.MaxSessions = sessionsOf(t, repo, stream) + 2
		},
		reach: func(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) error {
			recordSessions(t, f, repo, stream, 3)
			return nil
		},
		reason: "the loop guard",
	},
}

type limitRoute struct {
	f           *shedFixture
	stream      config.WorkstreamID
	repo        *trace.Repository
	x           router.Exit
	id          string
	limit       exhaustion
	way         string
	res         router.Result
	before      trace.WorkflowState
	transitions int
}

func routeWhileJevAnswers(t *testing.T, limit, way string) *limitRoute {
	t.Helper()
	ways := map[string]func(reach func() error) *jevfixture.Transitions{
		"the default fits": func(reach func() error) *jevfixture.Transitions {
			return &jevfixture.Transitions{Wait: func(context.Context) error { return reach() }}
		},
		"Jev departs": func(reach func() error) *jevfixture.Transitions {
			return &jevfixture.Transitions{
				Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, UnitReviewing, 0.95, 0.95) },
				Wait:   func(context.Context) error { return reach() },
			}
		},
		"the degraded default": func(reach func() error) *jevfixture.Transitions {
			return &jevfixture.Transitions{
				Wait: func(context.Context) error { return reach() },
				Fail: func(int) error { return &systemone.Error{Kind: systemone.KindTimeout, Message: "slow"} },
			}
		},
	}
	l := exhausted[limit]
	name := strings.NewReplacer(" ", "-").Replace(limit + way)
	f, stream, repo := newReviewFixture(t, "apply-limit-"+name)
	openRuntime(t, f, stream)
	l.arm(t, f, repo, stream)
	var reachErr error
	p := ways[way](func() error {
		reachErr = l.reach(t, f, repo, stream)
		return reachErr
	})
	f.s.router.Judge = jevfixture.TransitionJudge(f.clock.Now, p)
	x, id := sendBackExit(t, f, repo, stream, "checks-"+name)
	r := &limitRoute{f: f, stream: stream, repo: repo, x: x, id: id, limit: l, way: way, before: unitStateOf(t, repo, stream), transitions: len(workTransitions(t, repo, stream))}
	res, err := f.s.RouteExit(context.Background(), x)
	fixturekit.Must(t, err)
	fixturekit.Must(t, reachErr)
	r.res = res
	return r
}

var jevAnswers = []string{"the default fits", "Jev departs", "the degraded default"}

func eachLimitAndAnswer(t *testing.T, ways []string, check func(t *testing.T, r *limitRoute)) {
	for limit := range exhausted {
		for _, way := range ways {
			t.Run(limit+" and "+way, func(t *testing.T) {
				t.Parallel()
				check(t, routeWhileJevAnswers(t, limit, way))
			})
		}
	}
}

func TestALimitReachedWhileJevAnswersRefusesTheMoveAndHoldsTheExit(t *testing.T) {
	t.Parallel()
	eachLimitAndAnswer(t, jevAnswers, func(t *testing.T, r *limitRoute) {
		if r.res.Committed || r.res.Journal != router.StateRefused || r.res.Outcome != router.OutcomeHeld {
			t.Fatalf("result %+v", r.res)
		}
	})
}

func TestALimitReachedWhileJevAnswersLeavesTheUnitWhereItWas(t *testing.T) {
	t.Parallel()
	eachLimitAndAnswer(t, jevAnswers, func(t *testing.T, r *limitRoute) {
		if now := unitStateOf(t, r.repo, r.stream); now != r.before {
			t.Fatalf("the unit moved from %+v to %+v", r.before, now)
		}
		if got := len(workTransitions(t, r.repo, r.stream)); got != r.transitions {
			t.Fatalf("%d transitions were committed", got-r.transitions)
		}
	})
}

func TestALimitReachedWhileJevAnswersRunsNoEffectOfTheTarget(t *testing.T) {
	t.Parallel()
	eachLimitAndAnswer(t, jevAnswers, func(t *testing.T, r *limitRoute) {
		outbox, err := r.repo.Outbox(r.stream)
		fixturekit.Must(t, err)
		for _, e := range outbox {
			if e.TransitionID == r.id || strings.Contains(e.Event.Body, "checks failed") {
				t.Fatalf("an effect of the target ran: %+v", e)
			}
		}
	})
}

func TestALimitReachedWhileJevAnswersSendsOneRefusalNoticeEvenWhenRoutedAgain(t *testing.T) {
	t.Parallel()
	eachLimitAndAnswer(t, jevAnswers, func(t *testing.T, r *limitRoute) {
		notices := refusedNoticesOf(t, r.repo, r.stream)
		if len(notices) != 1 || !strings.Contains(notices[0], r.limit.reason) {
			t.Fatalf("notices %q", notices)
		}
		if again, err := r.f.s.RouteExit(context.Background(), r.x); err != nil || !again.Reused || len(refusedNoticesOf(t, r.repo, r.stream)) != 1 {
			t.Fatalf("routing the occurrence again: %+v, %v, %d notices", again, err, len(refusedNoticesOf(t, r.repo, r.stream)))
		}
	})
}

func TestALimitReachedWhileJevAnswersRecordsTheRefusalOnJevsJudgment(t *testing.T) {
	t.Parallel()
	eachLimitAndAnswer(t, jevAnswers[:2], func(t *testing.T, r *limitRoute) {
		if got := refusedJudgmentsOf(t, r.repo, r.stream); len(got) != 1 || !strings.Contains(got[0].Refused, r.limit.reason) {
			t.Fatalf("refused judgments %+v", got)
		}
	})
}

type limitedRecoveryApproval struct {
	m           *moveFixture
	p           *jevfixture.Transitions
	l           exhaustion
	req         RecoveryRequest
	before      trace.WorkflowState
	transitions int
	asked       int
	fail        *APIError
}

func approveRecoveryWhileLimited(t *testing.T, limit string) *limitedRecoveryApproval {
	t.Helper()
	l := exhausted[limit]
	m := newMoveFixture(t, "recovery-limit-"+strings.NewReplacer(" ", "-").Replace(limit))
	openRuntime(t, m.f, m.stream)
	p := &jevfixture.Transitions{}
	m.f.s.router.Judge = jevfixture.TransitionJudge(m.f.clock.Now, p)
	if _, e := m.f.s.ownerMove(context.Background(), string(m.stream), "resume", UnitMoveRequest{To: UnitContested, Note: "Hold it while I look."}); e != nil {
		t.Fatal(e.Message)
	}
	if out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"Review it again."}`); !strings.Contains(out, "pending as an owner decision on the recovery card") {
		t.Fatalf("the chief's move: %s", out)
	}
	cards := recoveryEntriesOf(t, m.f.s, m.repo, m.stream)
	if len(cards) != 1 || cards[0].Recovery == nil || cards[0].Recovery.Target != UnitReviewing {
		t.Fatalf("the cards %+v", cards)
	}
	req := recoveryRequestOf(t, cards[0])
	l.arm(t, m.f, m.repo, m.stream)
	fixturekit.Must(t, l.reach(t, m.f, m.repo, m.stream))
	a := &limitedRecoveryApproval{m: m, p: p, l: l, req: req, before: m.state(t), transitions: len(workTransitions(t, m.repo, m.stream)), asked: len(p.Requests())}
	_, a.fail = m.f.s.approveRecovery(context.Background(), string(m.stream), req)
	return a
}

func eachLimit(t *testing.T, check func(t *testing.T, limit string)) {
	for limit := range exhausted {
		t.Run(limit, func(t *testing.T) {
			t.Parallel()
			check(t, limit)
		})
	}
}

func TestApprovingARecoveryCardWhileALimitStandsIsRefusedAsHeld(t *testing.T) {
	t.Parallel()
	eachLimit(t, func(t *testing.T, limit string) {
		a := approveRecoveryWhileLimited(t, limit)
		if a.fail == nil || a.fail.Code != Conflict || !strings.Contains(a.fail.Message, "held") {
			t.Fatalf("approving while the limit stands = %+v", a.fail)
		}
	})
}

func TestARefusedRecoveryApprovalLeavesTheUnitWhereItWasWithoutAskingJev(t *testing.T) {
	t.Parallel()
	eachLimit(t, func(t *testing.T, limit string) {
		a := approveRecoveryWhileLimited(t, limit)
		if now := a.m.state(t); now != a.before || len(workTransitions(t, a.m.repo, a.m.stream)) != a.transitions {
			t.Fatalf("the unit moved from %+v to %+v", a.before, now)
		}
		if len(a.p.Requests()) != a.asked {
			t.Fatal("a pinned owner move asked Jev")
		}
	})
}

func TestARefusedRecoveryApprovalIsNoticedAndJournaledOnce(t *testing.T) {
	t.Parallel()
	eachLimit(t, func(t *testing.T, limit string) {
		a := approveRecoveryWhileLimited(t, limit)
		notices := refusedNoticesOf(t, a.m.repo, a.m.stream)
		if len(notices) != 1 || !strings.Contains(notices[0], a.l.reason) {
			t.Fatalf("notices %q", notices)
		}
		var refused int
		for _, rec := range routeRecordsOf(t, a.m.repo, a.m.stream) {
			if rec.Trigger == execution.TriggerMoveOwner && rec.State == router.StateRefused && rec.Authority == router.AuthorityOwner && rec.Target == UnitReviewing {
				refused++
			}
		}
		if refused != 1 {
			t.Fatalf("%d refused owner moves: %+v", refused, routeRecordsOf(t, a.m.repo, a.m.stream))
		}
	})
}

func TestARefusedRecoveryApprovalLeavesTheCardOpen(t *testing.T) {
	t.Parallel()
	eachLimit(t, func(t *testing.T, limit string) {
		a := approveRecoveryWhileLimited(t, limit)
		if got := recoveryEntriesOf(t, a.m.f.s, a.m.repo, a.m.stream); len(got) != 1 {
			t.Fatalf("a refused approval closed the card: %+v", got)
		}
	})
}

func TestApprovingTheRecoveryCardOnceTheLimitClearsMovesTheUnit(t *testing.T) {
	t.Parallel()
	eachLimit(t, func(t *testing.T, limit string) {
		a := approveRecoveryWhileLimited(t, limit)
		m := a.m
		fixturekit.Must(t, m.f.s.store.ClearPause(workstreamTarget(m.f, m.stream), runtime.PauseOwner))
		m.f.s.cfg.Loop.MaxSessions = 0
		m.f.clock.Mu.Lock()
		m.f.clock.At = m.f.clock.At.Add(time.Minute)
		m.f.clock.Mu.Unlock()
		if _, fail := m.f.s.approveRecovery(context.Background(), string(m.stream), a.req); fail != nil {
			t.Fatal(fail.Message)
		}
		moved := lastTransitionOf(t, m.repo, m.stream, trace.UnitSubject("resume"))
		if moved.To != UnitReviewing || moved.DecidedBy != trace.DecidedByOwner {
			t.Fatalf("the transition %+v", moved)
		}
		if len(refusedNoticesOf(t, m.repo, m.stream)) != 1 {
			t.Fatal("the approved move sent another refusal")
		}
	})
}
