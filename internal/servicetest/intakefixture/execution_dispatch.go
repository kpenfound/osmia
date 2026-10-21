package intakefixture

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const ReliabilityPlan = `{"version": 1, "units": [
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
const TrackedFile = "internal/trace/git.go"

func ReadUnitStatus(t *testing.T, client UnitStatusClient, stream config.WorkstreamID, unit string) api.UnitStatus {
	t.Helper()
	st, err := client.Status(context.Background(), stream)
	Must(t, err)
	for _, u := range st.Units {
		if u.Unit == unit {
			return u
		}
	}
	t.Fatalf("status of %s lists no unit %s: %+v", stream, unit, st.Units)
	return api.UnitStatus{}
}

func DeferredUnit(t *testing.T, repository *trace.Repository, pauses []runtime.Pause, stream config.WorkstreamID, unit string, d api.UnitDispatch) api.UnitStatus {
	t.Helper()
	if d.Pause != nil {
		for _, pause := range pauses {
			if pause.Target == d.Pause.Target {
				d.Pause = &pause
				break
			}
		}
	}
	states, err := repository.WorkflowStates(stream)
	Must(t, err)
	d.Unit, d.Version, d.Decision = unit, states[trace.UnitSubject(unit)].Version, "deferred"
	return api.UnitStatus{Unit: unit, State: "ready", Reason: d.Message, Deferral: &d}
}

func DispatchDecisions(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	documents, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	var out []string
	for _, doc := range documents {
		if doc.ID != trace.UnitSubject(unit)+"-dispatch" {
			continue
		}
		var d api.UnitDispatch
		Must(t, json.Unmarshal([]byte(doc.Content), &d))
		if doc.Revision != len(out)+1 || doc.Path != "units/"+unit+"/dispatch.json" || doc.Unit != unit || doc.Actor != intake.MasonActor || d.Unit != unit || d.Message == "" {
			t.Fatalf("dispatch revision %+v", doc)
		}
		out = append(out, strings.TrimSuffix(d.Decision+" "+d.Reason, " "))
	}
	return out
}

func AwaitDispatchDecisions(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, n int) {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for len(DispatchDecisions(t, repository, stream, unit)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("decisions on unit %s of %s: %q, want %d", unit, stream, DispatchDecisions(t, repository, stream, unit), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func MasonStarts(transitions []trace.Transition) []string {
	var out []string
	for _, tr := range transitions {
		if tr.Actor == intake.MasonActor && (strings.HasPrefix(tr.Subject, "unit-") || strings.HasPrefix(tr.Subject, "unit_") || strings.HasPrefix(tr.Subject, "blocked-mason")) && tr.From == "ready" && tr.To == "implementing" {
			out = append(out, tr.Subject)
		}
	}
	return out
}
func LowHigh(a, b config.WorkstreamID) (config.WorkstreamID, config.WorkstreamID) {
	return min(a, b), max(a, b)
}
