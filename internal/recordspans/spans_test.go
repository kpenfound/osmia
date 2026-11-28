package recordspans

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestWorkstreamStatesAreOneSpanEachWithNoGap(t *testing.T) {
	h := newHarness(t)
	h.feature("t1", "handed", minute(1), owner, "the owner handed it in")
	h.feature("t2", "sketched", minute(10), service, "sketch drafted")
	h.feature("t3", "ratified", minute(70), owner, "the owner ratified the sketch")
	h.feature("t4", "building", minute(71), service, "building")
	h.feature("t5", "delivered", minute(190), service, "delivered")

	states := named(h.spans(), "osmia.workstream.state")
	want := []struct {
		state, next, cause string
		start, end         time.Time
	}{
		{"created", "handed", "owner", created, minute(1)},
		{"handed", "sketched", "service", minute(1), minute(10)},
		{"sketched", "ratified", "owner", minute(10), minute(70)},
		{"ratified", "building", "service", minute(70), minute(71)},
		{"building", "delivered", "service", minute(71), minute(190)},
	}
	if len(states) != len(want) {
		t.Fatalf("%d state spans, want %d", len(states), len(want))
	}
	traceID, root := telemetry.WorkstreamTrace(stream), telemetry.WorkstreamRoot(stream)
	seen := map[string]bool{}
	for i, w := range want {
		s := states[i]
		if str(s, "osmia.state") != w.state || str(s, "osmia.next_state") != w.next || str(s, "osmia.cause") != w.cause || !s.StartTime.Equal(w.start) || !s.EndTime.Equal(w.end) {
			t.Fatalf("state span %d: %s, want %+v", i, describe(s), w)
		}
		if i > 0 && !s.StartTime.Equal(states[i-1].EndTime) {
			t.Fatalf("a gap between state spans %d and %d", i-1, i)
		}
		if s.SpanContext.TraceID() != traceID || s.Parent.SpanID() != root || str(s, "osmia.workstream.id") != string(stream) || str(s, "osmia.project.id") != string(project) {
			t.Fatalf("state span %d is not in the workstream's trace under its root: %s", i, describe(s))
		}
		if seen[s.SpanContext.SpanID().String()] {
			t.Fatalf("state span %d repeats a span ID", i)
		}
		seen[s.SpanContext.SpanID().String()] = true
	}
}

func TestUnitStateSpansCarryTheCauseAndNextState(t *testing.T) {
	h := newHarness(t)
	h.unit("u1-1", "u1", "ready", minute(1), service, "dependencies merged")
	h.unit("u1-2", "u1", "implementing", minute(2), mason, "dispatched")
	h.unit("u1-3", "u1", "checking", minute(10), mason, "candidate")
	h.unit("u1-4", "u1", "implementing", minute(15), checks, "the checks failed")
	h.unit("u1-5", "u1", "checking", minute(25), mason, "candidate")
	h.unit("u1-6", "u1", "reviewing", minute(30), checks, "checks passed")
	h.unit("u1-7", "u1", "implementing", minute(40), review, "material findings")
	h.unit("u1-8", "u1", "checking", minute(50), mason, "candidate")
	h.unit("u1-9", "u1", "reviewing", minute(52), checks, "checks passed")
	h.unit("u1-10", "u1", "approved", minute(60), review, "approved")

	units := named(h.spans(), "osmia.unit.state")
	want := []struct {
		state, next, cause string
		start, end         time.Time
	}{
		{"ready", "implementing", CauseDispatched, minute(1), minute(2)},
		{"implementing", "checking", CauseCandidateReported, minute(2), minute(10)},
		{"checking", "implementing", CauseChecksFailed, minute(10), minute(15)},
		{"implementing", "checking", CauseCandidateReported, minute(15), minute(25)},
		{"checking", "reviewing", CauseChecksPassed, minute(25), minute(30)},
		{"reviewing", "implementing", CauseReviewRejected, minute(30), minute(40)},
		{"implementing", "checking", CauseCandidateReported, minute(40), minute(50)},
		{"checking", "reviewing", CauseChecksPassed, minute(50), minute(52)},
		{"reviewing", "approved", CauseReviewApproved, minute(52), minute(60)},
	}
	if len(units) != len(want) {
		t.Fatalf("%d unit state spans, want %d", len(units), len(want))
	}
	returned := map[string]int{}
	for i, w := range want {
		s := units[i]
		if str(s, "osmia.state") != w.state || str(s, "osmia.next_state") != w.next || str(s, "osmia.cause") != w.cause || str(s, "osmia.unit.id") != "u1" || !s.StartTime.Equal(w.start) || !s.EndTime.Equal(w.end) {
			t.Fatalf("unit span %d: %s, want %+v", i, describe(s), w)
		}
		if s.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || s.Parent.SpanID() != telemetry.WorkstreamRoot(stream) {
			t.Fatalf("unit span %d is not in the workstream's trace: %s", i, describe(s))
		}
		if w.next == "implementing" && w.state != "ready" {
			returned[w.cause]++
		}
	}
	if returned[CauseChecksFailed] != 1 || returned[CauseReviewRejected] != 1 || len(returned) != 2 {
		t.Fatalf("returns to the mason by cause: %v", returned)
	}
}

func TestUnitCauseIsFromTheBoundedVocabulary(t *testing.T) {
	manager := trace.Actor{Kind: "agent", ID: trace.Manager}
	rebase := func(from, to string) trace.Transition {
		return trace.Transition{Header: trace.Header{Actor: service, Cause: "unit-u1-rebase-3-conflicted"}, From: from, To: to}
	}
	for _, c := range []struct {
		t    trace.Transition
		want string
	}{
		{trace.Transition{Header: trace.Header{Actor: service}, From: "planned", To: "ready"}, CauseDependenciesMet},
		{trace.Transition{Header: trace.Header{Actor: mason}, From: "ready", To: "implementing"}, CauseDispatched},
		{rebase("implementing", "implementing"), CauseRebaseConflict},
		{rebase("checking", "implementing"), CauseRebaseConflict},
		{trace.Transition{Header: trace.Header{Actor: service}, From: "implementing", To: "checking"}, CauseRebased},
		{trace.Transition{Header: trace.Header{Actor: review}, From: "reviewing", To: "checking"}, CauseReviewStale},
		{trace.Transition{Header: trace.Header{Actor: service}, From: "approved", To: "checking"}, CauseRebased},
		{trace.Transition{Header: trace.Header{Actor: checks}, From: "checking", To: "checking"}, CauseChecksRetry},
		{trace.Transition{Header: trace.Header{Actor: review}, From: "reviewing", To: "reviewing"}, CauseReviewRetry},
		{trace.Transition{Header: trace.Header{Actor: checks}, From: "checking", To: "contested"}, CauseChecksIncomplete},
		{trace.Transition{Header: trace.Header{Actor: review}, From: "reviewing", To: "contested"}, CauseReviewContested},
		{trace.Transition{Header: trace.Header{Actor: mason}, From: "implementing", To: "contested"}, CauseContested},
		{trace.Transition{Header: trace.Header{Actor: manager}, From: "contested", To: "implementing"}, CauseContestRuled},
		{trace.Transition{Header: trace.Header{Actor: owner}, From: "contested", To: "reviewing"}, CauseContestRuled},
		{trace.Transition{Header: trace.Header{Actor: manager}, From: "implementing", To: "approved"}, CauseMoved},
		{trace.Transition{Header: trace.Header{Actor: service}, From: "approved", To: "merged"}, CauseLanded},
		{trace.Transition{Header: trace.Header{Actor: service}, From: "implementing", To: "waiting"}, CauseAmendmentWait},
		{trace.Transition{Header: trace.Header{Actor: service}, From: "waiting", To: "implementing"}, CauseAmendmentResolved},
		{trace.Transition{Header: trace.Header{Actor: service}, From: "implementing", To: "something-new"}, CauseOther},
	} {
		got := UnitCause(c.t)
		if got != c.want {
			t.Errorf("%s -> %s by %s: cause %q, want %q", c.t.From, c.t.To, c.t.Actor.ID, got, c.want)
		}
		if !telemetry.Cause(got).Valid() {
			t.Errorf("cause %q is not an enumerated word", got)
		}
	}
	for _, cause := range UnitCauses {
		if !telemetry.Cause(cause).Valid() {
			t.Errorf("listed cause %q is not an enumerated word", cause)
		}
	}
	if UnitCause(trace.Transition{Header: trace.Header{Actor: mason}, From: "implementing", To: "checking", Reason: "Please fix the Thing"}) == "Please fix the Thing" {
		t.Fatal("a cause repeats the reason")
	}
}

func TestTurnSpansFollowTheGenAIConventions(t *testing.T) {
	h := newHarness(t)
	h.turn("t1", minute(1), minute(3), func(_ *trace.TurnRequest, r *trace.TurnResponse) {
		r.Result.Usage = coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 2}
	})
	h.turn("t2", minute(10), minute(10), func(_ *trace.TurnRequest, r *trace.TurnResponse) {
		r.Result.Usage = coreadapter.Usage{CostUSD: 0, CostKnown: false, Turns: 1}
		r.Result.Outcome = &coreadapter.Outcome{Status: "waiting", Report: "a question"}
	})
	h.turn("t3", minute(20), minute(21), func(_ *trace.TurnRequest, r *trace.TurnResponse) {
		r.Failure, r.FailureClass, r.Result.IsError = "it broke", coreadapter.Infrastructure, true
	})

	turns := named(h.spans(), "invoke_agent mason")
	if len(turns) != 3 {
		t.Fatalf("%d turn spans: %v", len(turns), h.spans())
	}
	first := turns[0]
	for key, want := range map[string]string{
		"gen_ai.operation.name": "invoke_agent", "gen_ai.provider.name": "fake", "gen_ai.request.model": "test-model-1",
		"gen_ai.agent.name": "mason", "gen_ai.agent.id": "mason",
		"osmia.role": "mason", "osmia.backend": "Fake", "osmia.model": "test-model-1", "osmia.profile": "default",
		"osmia.project.id": string(project), "osmia.workstream.id": string(stream), "osmia.unit.id": "u1",
		"osmia.outcome": TurnCompleted,
	} {
		if got := str(first, key); got != want {
			t.Errorf("attribute %s = %q, want %q", key, got, want)
		}
	}
	if v, ok := value(first, "osmia.slot_wait_ms"); !ok || v.AsInt64() != 2*60*1000 {
		t.Errorf("slot wait %v %v, want two minutes", v, ok)
	}
	if !first.StartTime.Equal(minute(3)) || !first.EndTime.Equal(minute(3).Add(5*time.Second)) {
		t.Errorf("turn times %v..%v", first.StartTime, first.EndTime)
	}
	if v, ok := value(first, "osmia.cost_usd"); !ok || v.AsFloat64() != 0.25 {
		t.Errorf("cost %v %v", v, ok)
	}
	if v, ok := value(first, "osmia.cost_known"); !ok || !v.AsBool() {
		t.Errorf("cost_known %v %v", v, ok)
	}
	if first.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || first.Parent.SpanID() != telemetry.WorkstreamRoot(stream) {
		t.Errorf("turn span is not in the workstream's trace: %s", describe(first))
	}
	second := turns[1]
	if _, ok := value(second, "osmia.cost_usd"); ok {
		t.Errorf("a turn the backend reported no cost for has a cost: %s", describe(second))
	}
	if v, ok := value(second, "osmia.cost_known"); !ok || v.AsBool() {
		t.Errorf("cost_known %v %v, want false", v, ok)
	}
	if str(second, "osmia.outcome") != TurnWaiting {
		t.Errorf("waiting turn outcome %q", str(second, "osmia.outcome"))
	}
	if v, _ := value(second, "osmia.slot_wait_ms"); v.AsInt64() != 0 {
		t.Errorf("a turn claimed when queued waited %d ms", v.AsInt64())
	}
	third := turns[2]
	if str(third, "osmia.outcome") != TurnFailed || str(third, "osmia.cause") != "infrastructure" || third.Status.Code.String() != "Error" {
		t.Errorf("failed turn: %s status %v", describe(third), third.Status)
	}
	for _, s := range turns {
		for _, kv := range s.Attributes {
			if strings.HasPrefix(string(kv.Key), "gen_ai.") && !pinnedGenAIKey(string(kv.Key)) {
				t.Errorf("attribute %s is not of the pinned conventions", kv.Key)
			}
			if strings.Contains(string(kv.Key), "token") {
				t.Errorf("a token attribute %s", kv.Key)
			}
		}
	}
	if telemetry.GenAIConventionsVersion != "1.40.0" || !strings.HasSuffix(telemetry.GenAISchemaURL, "/1.40.0") {
		t.Errorf("pinned version %s %s", telemetry.GenAIConventionsVersion, telemetry.GenAISchemaURL)
	}
}

func pinnedGenAIKey(key string) bool {
	switch key {
	case telemetry.KeyGenAIOperation, telemetry.KeyGenAIProvider, telemetry.KeyGenAIModel, telemetry.KeyGenAIAgentName, telemetry.KeyGenAIAgentID:
		return true
	}
	return false
}

func TestATurnOnAClientCarriesTheClientIdentityAndALocalOneThatNamesNoClientDoesNot(t *testing.T) {
	h := newHarness(t)
	h.turn("remote", minute(1), minute(2), func(_ *trace.TurnRequest, r *trace.TurnResponse) { r.Result.Client = "nREMOTE1CNTRL" })
	h.turn("unplaced", minute(5), minute(6), nil)
	turns := named(h.spans(), "invoke_agent mason")
	if len(turns) != 2 {
		t.Fatalf("%d turn spans: %v", len(turns), h.spans())
	}
	if got := str(turns[0], telemetry.KeyClient); got != "nREMOTE1CNTRL" {
		t.Errorf("the remote turn's client attribute %q: %s", got, describe(turns[0]))
	}
	if _, ok := value(turns[1], telemetry.KeyClient); ok {
		t.Errorf("a turn that records no client carries one: %s", describe(turns[1]))
	}
}

func TestStoppedTurnIsCancelledWithTheStopCause(t *testing.T) {
	h := newHarness(t)
	h.turn("t1", minute(1), minute(2), func(_ *trace.TurnRequest, r *trace.TurnResponse) {
		r.Result.Cancelled, r.Result.Session = true, coreadapter.BackendSession{}
		r.Stop = &trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: "all", Source: "owner", Reason: "pause reason"}
	})
	turns := named(h.spans(), "invoke_agent mason")
	if len(turns) != 1 || str(turns[0], "osmia.outcome") != TurnCancelled || str(turns[0], "osmia.cause") != trace.TurnStopHardPause {
		t.Fatalf("stopped turn: %v", turns)
	}
}

func TestCheckRunSpansCarryOutcomeAndCoveredCount(t *testing.T) {
	h := newHarness(t)
	covered, seven := 12, 7
	started, finished := minute(5), minute(9)
	h.document("unit-u1-checks-1", "units/u1/checks-1.json", finished, checks, "u1", execution.UnitCheckRun{Unit: "u1", Run: 1, Status: "passed", Selection: &execution.CheckSelection{Mode: "selected", Covered: &covered}, StartedAt: started, FinishedAt: finished})
	h.document("unit-u1-checks-1-selection", "units/u1/checks-1-selection.json", started, checks, "u1", execution.UnitCheckSelection{Unit: "u1", Run: 1, Selection: execution.CheckSelection{Mode: "selected", Covered: &covered}, StartedAt: started})
	h.document("unit-u1-checks-2", "units/u1/checks-2.json", minute(20), checks, "u1", execution.UnitCheckRun{Unit: "u1", Run: 2, Status: "failed", Selection: &execution.CheckSelection{Mode: "full", Reason: "disabled"}, StartedAt: minute(15), FinishedAt: minute(20)})
	h.document("final-checks-1", "final/checks-1.json", minute(40), checks, "", delivery.FinalCheckRun{Review: 1, Status: "passed", StartedAt: minute(30), FinishedAt: minute(40)})
	h.document("session-checks-1", "session-checks/checks-1.json", minute(60), owner, "", api.SessionCheckRun{Run: 1, Kind: "smart", Status: "incomplete", StartedAt: minute(50), FinishedAt: minute(60), Selection: &api.CheckSelection{Mode: "selected", Covered: &seven}})
	h.document("final-checks-1-output", "final/checks-1-output.txt", minute(40), checks, "", "output text")

	runs := named(h.spans(), "osmia.check.run")
	if len(runs) != 4 {
		t.Fatalf("%d check run spans, want 4: %v", len(runs), runs)
	}
	for i, w := range []struct {
		kind, outcome, covered string
		unit                   string
		start, end             time.Time
		failed                 bool
	}{
		{"unit", "passed", "12", "u1", started, finished, false},
		{"unit", "failed", "", "u1", minute(15), minute(20), true},
		{"final", "passed", "", "", minute(30), minute(40), false},
		{"session", "incomplete", "7", "", minute(50), minute(60), true},
	} {
		s := runs[i]
		if str(s, "osmia.kind") != w.kind || str(s, "osmia.outcome") != w.outcome || str(s, "osmia.covered") != w.covered || str(s, "osmia.unit.id") != w.unit || !s.StartTime.Equal(w.start) || !s.EndTime.Equal(w.end) || (s.Status.Code.String() == "Error") != w.failed {
			t.Errorf("check run span %d: %s", i, describe(s))
		}
		if s.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || s.Parent.SpanID() != telemetry.WorkstreamRoot(stream) {
			t.Errorf("check run span %d is not in the workstream's trace", i)
		}
	}
	if _, ok := value(runs[1], "osmia.covered"); ok {
		t.Errorf("a run whose selection holds no count carries one")
	}
}

func TestRootSpanCoversTheWorkstreamLifeAndIsSentOnEnd(t *testing.T) {
	for _, how := range []string{Archived, Removed} {
		t.Run(how, func(t *testing.T) {
			h := newHarness(t)
			h.feature("t1", "handed", minute(1), owner, "handed")
			h.feature("t2", "delivered", minute(30), service, "delivered")
			if roots := named(h.spans(), "osmia.workstream"); len(roots) != 0 {
				t.Fatalf("a root span before the workstream ended: %v", roots)
			}
			end := minute(500)
			Recorder{Sink: h.sink}.Ended(h.repo, stream, end, how)
			spans := h.spans()
			roots := named(spans, "osmia.workstream")
			if len(roots) != 1 {
				t.Fatalf("%d root spans", len(roots))
			}
			root := roots[0]
			if root.Parent.IsValid() || root.SpanContext.SpanID() != telemetry.WorkstreamRoot(stream) || !root.StartTime.Equal(created) || !root.EndTime.Equal(end) ||
				str(root, "osmia.outcome") != "delivered" || str(root, "osmia.cause") != how {
				t.Fatalf("root span: %s", describe(root))
			}
			states := named(spans, "osmia.workstream.state")
			last := states[len(states)-1]
			if str(last, "osmia.state") != "delivered" || str(last, "osmia.next_state") != how || !last.StartTime.Equal(minute(30)) || !last.EndTime.Equal(end) {
				t.Fatalf("last state span: %s", describe(last))
			}
			for _, s := range spans {
				if s.SpanContext.TraceID() != root.SpanContext.TraceID() {
					t.Fatalf("span %s is not in the root's trace", describe(s))
				}
			}
		})
	}
}

func TestEndedWithoutATimeOrSinkSendsNothing(t *testing.T) {
	h := newHarness(t)
	h.feature("t1", "handed", minute(1), owner, "handed")
	Recorder{Sink: h.sink}.Ended(h.repo, stream, time.Time{}, Removed)
	Recorder{}.Ended(h.repo, stream, minute(5), Archived)
	Recorder{Sink: &telemetry.Sink{}}.Ended(h.repo, stream, minute(5), Archived)
	if named(h.spans(), "osmia.workstream") != nil {
		t.Fatal("a root span was sent")
	}
}

func TestIntervalOpenAcrossARestartKeepsItsStartAndIDs(t *testing.T) {
	run := func(restart bool) tracetest.SpanStubs {
		h := newHarness(t)
		h.feature("t1", "handed", minute(1), owner, "handed")
		h.unit("u1-1", "u1", "ready", minute(2), service, "ready")
		h.unit("u1-2", "u1", "implementing", minute(3), mason, "dispatched")
		if restart {
			h.restart()
		}
		h.feature("t2", "sketched", minute(20), service, "sketched")
		h.unit("u1-3", "u1", "checking", minute(25), mason, "candidate")
		return h.spans()
	}
	plain, restarted := run(false), run(true)
	for _, name := range []string{"osmia.workstream.state", "osmia.unit.state"} {
		a, b := named(plain, name), named(restarted, name)
		if len(a) == 0 || len(a) != len(b) {
			t.Fatalf("%s spans: %d without a restart, %d with", name, len(a), len(b))
		}
		for i := range a {
			if a[i].SpanContext.TraceID() != b[i].SpanContext.TraceID() || a[i].SpanContext.SpanID() != b[i].SpanContext.SpanID() || a[i].Parent.SpanID() != b[i].Parent.SpanID() || !a[i].StartTime.Equal(b[i].StartTime) || !a[i].EndTime.Equal(b[i].EndTime) {
				t.Fatalf("%s span %d differs across a restart:\n%s\n%s", name, i, describe(a[i]), describe(b[i]))
			}
		}
	}
	closing := named(restarted, "osmia.workstream.state")
	last := closing[len(closing)-1]
	if !last.StartTime.Equal(minute(1)) || !last.EndTime.Equal(minute(20)) || last.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || last.Parent.SpanID() != telemetry.WorkstreamRoot(stream) {
		t.Fatalf("the span closing after the restart: %s", describe(last))
	}
	if want := telemetry.WorkstreamSpan(stream, "workstream-state", "t1"); last.SpanContext.SpanID() != want {
		t.Fatalf("span ID %s, want %s", last.SpanContext.SpanID(), want)
	}
	units := named(restarted, "osmia.unit.state")
	if open := units[len(units)-1]; !open.StartTime.Equal(minute(3)) || str(open, "osmia.state") != "implementing" {
		t.Fatalf("the unit span closing after the restart: %s", describe(open))
	}
}

func TestSpanTimesAreRecordedTimes(t *testing.T) {
	h := newHarness(t)
	h.feature("t1", "handed", minute(1), owner, "handed")
	h.feature("t2", "sketched", minute(2), owner, "sketched")
	for _, s := range h.spans() {
		if s.EndTime.After(minute(2)) || s.StartTime.Before(created) {
			t.Fatalf("span %s has a time outside the recorded ones", describe(s))
		}
	}
}

func TestObserverPanicNeverFailsACommit(t *testing.T) {
	h := newHarness(t)
	h.repo.ObserveSpans(func(trace.CommitDetail) { panic("observer failed") })
	h.feature("t1", "handed", minute(1), owner, "handed")
	h.feature("t2", "sketched", minute(2), owner, "sketched")
	if state, err := h.repo.Workflow(stream, trace.FeatureSubject); err != nil || state.Value != "sketched" {
		t.Fatalf("state %v %v", state, err)
	}
}

func TestAManagerTurnSpanReportsTheManagerRole(t *testing.T) {
	h := newHarness(t)
	h.turnOf(trace.Manager, "m1", minute(1), minute(2), func(req *trace.TurnRequest, resp *trace.TurnResponse) {
		req.Unit, resp.Unit = "", ""
	})

	turns := named(h.spans(), "invoke_agent "+trace.ManagerDisplay)
	if len(turns) != 1 {
		t.Fatalf("%d manager turn spans: %v", len(turns), h.spans())
	}
	for key, want := range map[string]string{
		"gen_ai.agent.name": trace.ManagerDisplay, "gen_ai.agent.id": trace.ManagerDisplay, "osmia.role": trace.ManagerDisplay,
	} {
		if got := str(turns[0], key); got != want {
			t.Errorf("attribute %s = %q, want %q", key, got, want)
		}
	}
	if len(named(h.spans(), "invoke_agent "+trace.Manager)) != 0 {
		t.Fatal("a turn span carries the stored name of the manager")
	}
}

func TestAManagerEndingAWorkstreamStateIsTheManagerCause(t *testing.T) {
	if got := FeatureCause(trace.Transition{Header: trace.Header{Actor: trace.Actor{Kind: "agent", ID: trace.Manager}}}); got != trace.ManagerDisplay {
		t.Fatalf("cause %q, want %q", got, trace.ManagerDisplay)
	}
}
