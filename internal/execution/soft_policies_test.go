package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// routeRecord returns the latest revision of the route journal of the exit with
// the trigger.
func routeRecord(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "routes/") && d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	for _, d := range latest {
		var rec router.Record
		must(t, json.Unmarshal([]byte(d.Content), &rec))
		if rec.Trigger == trigger {
			return rec
		}
	}
	t.Fatalf("no exit of trigger %s was routed", trigger)
	return router.Record{}
}

func lastUnitTransition(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	var out trace.Transition
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject("resume") {
			out = tr
		}
	}
	return out
}

// departsTo scripts a Jev that finds the default does not fit and picks target
// with every question of the trigger corroborating it.
func departsTo(target string) func(jevfixture.Ask) jevfixture.Reply {
	return func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, target, 0.9, 0.9) }
}

// softLimitCases are the two ways Jev can answer an exit whose default is the
// outcome of a policy limit: the default fits, or the other legal target is the
// better one.
type softLimitCase struct {
	name      string
	script    func(jevfixture.Ask) jevfixture.Reply
	to        string
	decidedBy string
	departed  bool
}

func softLimitCases(departTo string) []softLimitCase {
	return []softLimitCase{
		{"the default fits", nil, UnitContested, trace.DecidedByCode, false},
		{"a scripted departure", departsTo(departTo), departTo, trace.DecidedByJev, true},
	}
}

// requireDeparture checks what a departure past a limit records: the state it
// entered, the default it left, Jev as the decider with the judgment, and the
// reason, in the transition and in the route journal.
func requireDeparture(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string, tc softLimitCase) {
	t.Helper()
	moved := lastUnitTransition(t, repo, stream)
	rec := routeRecord(t, repo, stream, trigger)
	if moved.To != tc.to || moved.DefaultTo != UnitContested || moved.DecidedBy != tc.decidedBy || moved.Departed() != tc.departed || moved.Judgment == "" {
		t.Fatalf("the transition %+v; the route %+v", moved, rec)
	}
	if rec.State != router.StateCommitted || rec.Default != UnitContested || rec.Target != tc.to || rec.Judgment != moved.Judgment {
		t.Fatalf("the route %+v", rec)
	}
	if !tc.departed {
		if rec.Outcome != router.OutcomeFollowed || strings.Contains(moved.Reason, "decide-transition") {
			t.Fatalf("a default that was followed reads as its controller wrote it: %+v, %q", rec, moved.Reason)
		}
		return
	}
	// The departure records why: the rule of the table that chose it and the
	// answers of Jev behind it.
	if rec.Outcome != router.OutcomeDeparted || rec.DecidedBy != trace.DecidedByJev || rec.Rule != "6:departure" || rec.Reason == "" ||
		!strings.Contains(moved.Reason, "decide-transition table "+router.TableVersion+", rule 6:departure") {
		t.Fatalf("the departure is not recorded with its reason: %+v, %q", rec, moved.Reason)
	}
}

// The send-back count reaching shed.max_bounces sets the default of a failed
// check run to contested, and the other legal targets stay offered. With a Jev
// that finds the default fits the unit is contested as it always was; a
// scripted departure sends it back to its mason past the limit, and the
// departure records its reason.
func TestTheSendBackCountSetsTheDefaultOfAFailedCheckRun(t *testing.T) {
	t.Parallel()
	failing := "dag://go/packages/tests/test?go-package=internal/trace"
	for _, tc := range softLimitCases(UnitImplementing) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checks := &fakeChecks{Outcomes: []checkOutcome{{Result: api.CheckResult{ExitCode: 1, Output: failedReport(failing)}}}}
			f, stream, repo := newChecksFixture(t, "send-back-limit", checks)
			f.cfg.Shed.MaxBounces = 1
			f.jev.Script = tc.script
			runChecks(t, f.s, repo, stream)

			if got := unitState(t, repo, stream, "resume"); got != tc.to {
				t.Fatalf("the unit is %s, want %s", got, tc.to)
			}
			requireDeparture(t, repo, stream, TriggerChecksFailed, tc)
			rec := routeRecord(t, repo, stream, TriggerChecksFailed)
			if !contains(rec.Offered, UnitImplementing) || !contains(rec.Offered, UnitChecking) || !contains(rec.Offered, UnitContested) {
				t.Fatalf("the limit left only %v on offer", rec.Offered)
			}
			if tc.departed {
				if ruling, found, err := latestMasonRuling(repo, stream, "resume"); err != nil || !found || ruling.ResetTurn == 0 {
					t.Fatalf("the departure gave the mason no fresh allowance: %+v %v %v", ruling, found, err)
				}
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// A mason whose turns ended without a report mason.max_clean_turns times sets
// the default of the unit's exit to contested, with the mason's own turn as
// another target. A departure gives the mason a turn with a fresh clean-turn
// allowance, and records its reason.
func TestTheCleanTurnLimitSetsTheDefaultOfAnEndingWithoutAReport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range softLimitCases(UnitImplementing) {
		if tc.departed {
			// The mason's turn needs recent turns to have changed the work and
			// the mason not to be blocked on a human.
			tc.script = func(a jevfixture.Ask) jevfixture.Reply {
				r := jevfixture.Departs(a, UnitImplementing, 0.9, 0.9)
				r.Own["needs_human"] = 0.1
				return r
			}
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBuildFixture(t, "clean-turn-limit")
			f.jev.Script = tc.script
			m := f.controller()
			b, found, err := m.read(stream)
			must(t, err)
			if !found {
				t.Fatal("fixture workstream is not building")
			}
			if started, _, err := m.start(ctx, b, "resume"); err != nil || !started {
				t.Fatalf("the unit did not start: %v", err)
			}
			state, err := f.repository.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			th, err := f.repository.Thread(stream, masonAgent("resume"))
			must(t, err)
			first := th.Turns[0]
			last := trace.QueuedTurn{Sequence: first.Sequence, Request: first.Request, Response: &trace.TurnResponse{Header: trace.Header{ID: "response-clean-turn"}}}
			attempts := f.cfg.Mason.MaxCleanTurns
			must(t, m.contestEnding(ctx, stream, "resume", state, last, "unit-resume-contested-clean-turn", "mason clean-turn bound exhausted after unclear", attempts, "the mason worked on the parser"))

			if got := unitState(t, f.repository, stream, "resume"); got != tc.to && !(tc.departed && got == UnitImplementing) {
				t.Fatalf("the unit is %s, want %s", got, tc.to)
			}
			requireDeparture(t, f.repository, stream, TriggerMasonEndedClean, tc)
			rec := routeRecord(t, f.repository, stream, TriggerMasonEndedClean)
			if !contains(rec.Offered, UnitContested) || !contains(rec.Offered, UnitImplementing) {
				t.Fatalf("the limit left only %v on offer", rec.Offered)
			}
			if tc.departed {
				ruling, found, err := latestMasonRuling(f.repository, stream, "resume")
				if err != nil || !found || ruling.ResetTurn != first.Sequence {
					t.Fatalf("the departure gave the mason no fresh allowance: %+v %v %v", ruling, found, err)
				}
			}
		})
	}
}
