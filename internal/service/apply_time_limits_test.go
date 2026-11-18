package service

import (
	"context"
	"encoding/json"
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

// refusedNoticesOf returns the bodies of the notices the router sent the chief
// of staff for the moves it refused.
func refusedNoticesOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []string {
	t.Helper()
	outbox, err := repo.Outbox(stream)
	must(t, err)
	var out []string
	for _, e := range outbox {
		if strings.HasPrefix(e.TransitionID, "route-") && strings.HasSuffix(e.TransitionID, "-refused") {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

// refusedJudgmentsOf returns the judgments of the workstream that were marked
// refused.
func refusedJudgmentsOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []jev.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "judgments/") && d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []jev.Record
	for _, d := range latest {
		var r jev.Record
		must(t, json.Unmarshal([]byte(d.Content), &r))
		if r.Refused != "" {
			out = append(out, r)
		}
	}
	return out
}

// sendBackExit is the exit of the unit resume whose checks failed, as the
// checkers raise it: today's default sends the unit back to its mason with
// entry effects of its own, and Jev may instead have it reviewed again.
func sendBackExit(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, cause string) (router.Exit, string) {
	t.Helper()
	id := "unit-resume-implementing-" + cause
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: execution.TriggerChecksFailed, Cause: cause,
		Default: UnitImplementing, Targets: []string{UnitImplementing, UnitChecking, UnitReviewing, UnitContested}, Actor: foremanActor,
		ID: id, HeaderCause: cause, Reason: "check run 1 failed", Note: "check run 1 failed",
		Events: []trace.Event{trace.Notice(id, "unit", "Unit resume checks failed: check run 1 failed.")}})
	must(t, err)
	return x, id
}

// unitStateOf returns the state of the unit resume.
func unitStateOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) trace.WorkflowState {
	t.Helper()
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	return state
}

// exhausted are the hard limits a move can reach while Jev answers: the daily
// budget's pause, and the loop guard's count of sessions. arm sets the limit up
// and reach makes it reached.
var exhausted = map[string]struct {
	arm    func(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID)
	reach  func(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) error
	reason string
}{
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

// A budget or loop limit that is reached while Jev answers stops an ordinary
// unit exit however its target was chosen: the default that carries its own
// entry effects, a departure Jev picked, and the default a degraded Jev leaves.
// Refuse is recorded, nothing is committed, no effect of the target runs, and
// the chief of staff gets exactly one notice.
func TestALimitReachedWhileJevAnswersStopsTheMove(t *testing.T) {
	t.Parallel()
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
	for limit, l := range exhausted {
		for way, script := range ways {
			t.Run(limit+" and "+way, func(t *testing.T) {
				t.Parallel()
				f, stream, repo := newReviewFixture(t, "apply-limit-"+strings.NewReplacer(" ", "-").Replace(limit+way))
				openRuntime(t, f, stream)
				l.arm(t, f, repo, stream)
				var reachErr error
				p := script(func() error {
					reachErr = l.reach(t, f, repo, stream)
					return reachErr
				})
				f.s.router.Judge = jevfixture.TransitionJudge(f.clock.Now, p)
				x, id := sendBackExit(t, f, repo, stream, "checks-"+strings.NewReplacer(" ", "-").Replace(limit+way))
				before, transitions := unitStateOf(t, repo, stream), len(workTransitions(t, repo, stream))

				res, err := f.s.RouteExit(context.Background(), x)
				must(t, err)
				must(t, reachErr)
				if res.Committed || res.Journal != router.StateRefused || res.Outcome != router.OutcomeHeld {
					t.Fatalf("result %+v", res)
				}
				if now := unitStateOf(t, repo, stream); now != before {
					t.Fatalf("the unit moved from %+v to %+v", before, now)
				}
				if got := len(workTransitions(t, repo, stream)); got != transitions {
					t.Fatalf("%d transitions were committed", got-transitions)
				}
				outbox, err := repo.Outbox(stream)
				must(t, err)
				for _, e := range outbox {
					if e.TransitionID == id || strings.Contains(e.Event.Body, "checks failed") {
						t.Fatalf("an effect of the target ran: %+v", e)
					}
				}
				notices := refusedNoticesOf(t, repo, stream)
				if len(notices) != 1 || !strings.Contains(notices[0], l.reason) {
					t.Fatalf("notices %q", notices)
				}
				if again, err := f.s.RouteExit(context.Background(), x); err != nil || !again.Reused || len(refusedNoticesOf(t, repo, stream)) != 1 {
					t.Fatalf("routing the occurrence again: %+v, %v, %d notices", again, err, len(refusedNoticesOf(t, repo, stream)))
				}
				// A degraded Jev leaves no judgment to refuse; an answered pick is
				// refused on its judgment.
				if way != "the degraded default" {
					if got := refusedJudgmentsOf(t, repo, stream); len(got) != 1 || !strings.Contains(got[0].Refused, l.reason) {
						t.Fatalf("refused judgments %+v", got)
					}
				}
			})
		}
	}
}

// Approving a recovery card while a hard limit stays reached records Refuse on
// the pinned move, commits nothing, runs none of its effects and sends the chief
// of staff exactly one notice. The card stays open, and approving it once the
// limit is lifted applies the move as the owner.
func TestApprovingARecoveryCardWhileALimitStandsIsRefused(t *testing.T) {
	t.Parallel()
	for limit, l := range exhausted {
		t.Run(limit, func(t *testing.T) {
			t.Parallel()
			m := newMoveFixture(t, "recovery-limit-"+strings.NewReplacer(" ", "-").Replace(limit))
			openRuntime(t, m.f, m.stream)
			// The owner holds the unit, so the chief of staff's move out of that
			// contest waits on the recovery card.
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
			must(t, l.reach(t, m.f, m.repo, m.stream))
			before, transitions, asked := m.state(t), len(workTransitions(t, m.repo, m.stream)), len(p.Requests())

			_, fail := m.f.s.approveRecovery(context.Background(), string(m.stream), req)
			if fail == nil || fail.Code != Conflict || !strings.Contains(fail.Message, "held") {
				t.Fatalf("approving while the limit stands = %+v", fail)
			}
			if now := m.state(t); now != before || len(workTransitions(t, m.repo, m.stream)) != transitions {
				t.Fatalf("the unit moved from %+v to %+v", before, now)
			}
			if len(p.Requests()) != asked {
				t.Fatal("a pinned owner move asked Jev")
			}
			notices := refusedNoticesOf(t, m.repo, m.stream)
			if len(notices) != 1 || !strings.Contains(notices[0], l.reason) {
				t.Fatalf("notices %q", notices)
			}
			var refused int
			for _, rec := range routeRecordsOf(t, m.repo, m.stream) {
				if rec.Trigger == execution.TriggerMoveOwner && rec.State == router.StateRefused && rec.Authority == router.AuthorityOwner && rec.Target == UnitReviewing {
					refused++
				}
			}
			if refused != 1 {
				t.Fatalf("%d refused owner moves: %+v", refused, routeRecordsOf(t, m.repo, m.stream))
			}
			if got := recoveryEntriesOf(t, m.f.s, m.repo, m.stream); len(got) != 1 {
				t.Fatalf("a refused approval closed the card: %+v", got)
			}

			// Lifted, the same card applies the move as the owner.
			must(t, m.f.s.store.ClearPause(workstreamTarget(m.f, m.stream), runtime.PauseOwner))
			m.f.s.cfg.Loop.MaxSessions = 0
			m.f.clock.Mu.Lock()
			m.f.clock.At = m.f.clock.At.Add(time.Minute)
			m.f.clock.Mu.Unlock()
			if _, fail := m.f.s.approveRecovery(context.Background(), string(m.stream), req); fail != nil {
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
}
