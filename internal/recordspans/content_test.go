package recordspans

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const marker = "MARKER-7f3a9c"

func TestNoContentReachesAnySpan(t *testing.T) {
	h := newHarness(t)
	h.feature("t1", "handed", minute(1), owner, "reason "+marker)
	h.feature("t2", "sketched", minute(10), owner, "reason "+marker+" title "+marker)
	h.unit("u1-1", "u1", "ready", minute(11), service, "reason "+marker)
	h.unit("u1-2", "u1", "implementing", minute(12), mason, "reason "+marker)
	h.unit("u1-3", "u1", "checking", minute(20), mason, "reason "+marker)
	h.unit("u1-4", "u1", "implementing", minute(21), checks, "reason "+marker)
	h.turn("t1", minute(2), minute(3), func(req *trace.TurnRequest, r *trace.TurnResponse) {
		req.Prompt, req.SystemPrompt, req.History = "prompt "+marker, "system "+marker, "history "+marker
		r.Result.FinalResponse = "reply " + marker
		r.Result.Outcome = &coreadapter.Outcome{Status: "waiting", Report: "question " + marker}
		r.Advice = "advice " + marker
	})
	h.turn("t2", minute(30), minute(31), func(req *trace.TurnRequest, r *trace.TurnResponse) {
		req.Prompt = "prompt " + marker
		r.Failure, r.Result.IsError, r.Result.ErrorSubtype = "failure "+marker, true, "subtype "+marker
	})
	h.turn("t3", minute(40), minute(41), func(req *trace.TurnRequest, r *trace.TurnResponse) {
		r.Result.Cancelled, r.Result.Session = true, coreadapter.BackendSession{}
		r.Result.FinalResponse = ""
		r.Stop = &trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: "all", Source: "owner", Reason: "pause " + marker}
	})
	noisy := []string{marker + "/a", marker + "/b"}
	h.document("unit-u1-checks-1", "units/u1/checks-1.json", minute(30), checks, "u1", execution.UnitCheckRun{Unit: "u1", Run: 1, Status: "failed", Output: "output " + marker, Error: "error " + marker, Command: noisy, Failed: noisy, Flaky: noisy,
		Selection: &execution.CheckSelection{Mode: "selected", Links: noisy, Detail: "detail " + marker, Reason: marker}, StartedAt: minute(25), FinishedAt: minute(30)})
	h.document("final-checks-1", "final/checks-1.json", minute(50), checks, "", delivery.FinalCheckRun{Review: 1, Status: "failed", Output: "output " + marker, Error: marker, Failed: noisy, StartedAt: minute(45), FinishedAt: minute(50)})
	h.document("session-checks-1", "session-checks/checks-1.json", minute(60), owner, "", api.SessionCheckRun{Run: 1, Kind: "smart", Status: "incomplete", Output: "output " + marker, Error: marker, Checks: noisy, StartedAt: minute(55), FinishedAt: minute(60)})
	h.document("spec-title", "spec.md", minute(5), owner, "", "# Title "+marker)
	Recorder{Sink: h.sink}.Ended(h.repo, stream, minute(500), Archived)

	spans := h.spans()
	if len(spans) != 13 {
		t.Fatalf("%d spans were sent, want 13: %v", len(spans), spans)
	}
	for _, s := range spans {
		if where := markerIn(s); where != "" {
			t.Errorf("span %q carries recorded text in its %s: %s", s.Name, where, describe(s))
		}
	}
}

func markerIn(s tracetest.SpanStub) string {
	has := func(text string) bool { return strings.Contains(strings.ToLower(text), strings.ToLower(marker)) }
	values := func(kvs []attribute.KeyValue) bool {
		for _, kv := range kvs {
			if has(string(kv.Key)) || has(kv.Value.String()) {
				return true
			}
			if kv.Value.Type() == attribute.STRINGSLICE {
				for _, v := range kv.Value.AsStringSlice() {
					if has(v) {
						return true
					}
				}
			}
		}
		return false
	}
	switch {
	case has(s.Name):
		return "name"
	case values(s.Attributes):
		return "attributes"
	case has(s.Status.Description):
		return "status"
	case values(s.Resource.Attributes()):
		return "resource"
	case has(s.InstrumentationScope.Name) || has(s.InstrumentationScope.Version):
		return "scope"
	}
	for _, e := range s.Events {
		if has(e.Name) || values(e.Attributes) {
			return "event"
		}
	}
	return ""
}
