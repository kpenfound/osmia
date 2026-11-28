package docs

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
)

//go:embed telemetry.md
var telemetryDoc string

func documented(t *testing.T, kind string, words ...string) {
	t.Helper()
	for _, w := range words {
		if !strings.Contains(telemetryDoc, "`"+w+"`") {
			t.Errorf("docs/telemetry.md does not list the %s `%s`", kind, w)
		}
	}
}

// Every span name, attribute key, cause and outcome the code can send is on the
// telemetry page, and so is the pinned GenAI conventions version.
func TestTelemetryPageListsWhatTheCodeSends(t *testing.T) {
	documented(t, "span name",
		telemetry.SpanName("workstream").String(),
		telemetry.SpanName("workstream", "state").String(),
		telemetry.SpanName("unit", "state").String(),
		telemetry.GenAIOperationInvokeAgent+" <role>",
		telemetry.SpanName("check", "run").String(),
		telemetry.SpanName("slot", string(slotspans.Held)).String(),
		telemetry.SpanName("slot", string(slotspans.Wait)).String(),
		telemetry.SpanName("pool", "limit").String(),
		telemetry.SpanName("pause").String(),
		telemetry.SpanName("service", "start").String(),
		telemetry.SpanName("service", "uptime").String(),
		telemetry.SpanName("loop_guard", "trip").String(),
		telemetry.SpanName("transition", "route").String(),
		telemetry.SpanName("verify", "unit").String(),
		telemetry.SpanName("verify", "final").String(),
		telemetry.SpanName("verify", "final_flow").String(),
		telemetry.SpanName("verify", "final_unit").String(),
	)
	documented(t, "attribute", telemetry.AttributeKeys()...)
	documented(t, "unit cause", recordspans.UnitCauses...)
	documented(t, "workstream cause", recordspans.CauseOwner, recordspans.CauseManager, recordspans.CauseService, recordspans.CauseAgent, recordspans.Archived, recordspans.Removed)
	documented(t, "turn outcome", recordspans.TurnCompleted, recordspans.TurnWaiting, recordspans.TurnFailed, recordspans.TurnCancelled)
	documented(t, "verification flow outcome", recordspans.FlowFail, recordspans.FlowInconclusive)
	documented(t, "check kind", recordspans.CheckUnit, recordspans.CheckFinal, recordspans.CheckSession)
	documented(t, "slot outcome", slotspans.Released, slotspans.Granted, slotspans.Canceled, slotspans.Removed, slotspans.Blocked, slotspans.Abandoned, slotspans.Interrupted)
	documented(t, "wait reason", slotspans.ReasonCapacity, slotspans.ReasonWorkstreamCap, slotspans.ReasonClient, slotspans.ReasonNoClient)
	documented(t, "factory outcome", "reloaded", "shutdown", "replaced")
	documented(t, "decided by value", trace.DecidedByCode, trace.DecidedByJev, trace.DecidedByFallback, trace.DecidedByOwner, trace.DecidedByJevGate, trace.ManagerDisplay, trace.DecidedByReviewer)
	documented(t, "route outcome", recordspans.RouteOutcomes...)
	documented(t, "confidence band", recordspans.ConfidenceBands...)
	documented(t, "route trigger", recordspans.RouteTriggers...)
	if !strings.Contains(telemetryDoc, "semantic conventions "+telemetry.GenAIConventionsVersion) {
		t.Errorf("docs/telemetry.md does not name GenAI conventions %s", telemetry.GenAIConventionsVersion)
	}
}

// The five questions have a worked query each, against the otel.otel_traces
// table the collector's ClickHouse exporter writes.
func TestTelemetryPageHasTheFiveQueries(t *testing.T) {
	if n := strings.Count(telemetryDoc, "```sql"); n != 5 {
		t.Fatalf("docs/telemetry.md has %d SQL queries, want 5", n)
	}
	for _, name := range []string{"osmia.pool.limit", "osmia.slot.held", "invoke_agent", "osmia.unit.state", "osmia.workstream.state"} {
		if !strings.Contains(telemetryDoc, "'"+name+"'") && !strings.Contains(telemetryDoc, "= '"+name) {
			t.Errorf("no query reads %s", name)
		}
	}
}

// The departure-rate query compares the units that departed from the default
// with those that followed it, from the routing attributes of the state spans.
func TestTelemetryPageDepartureQueryReadsTheRoutingAttributes(t *testing.T) {
	_, query, ok := strings.Cut(telemetryDoc, "### 5. Departure rate")
	if !ok {
		t.Fatal("docs/telemetry.md has no departure rate query")
	}
	for _, want := range []string{"osmia.unit.state", "'osmia.decided_by'] = 'jev'", "'osmia.deviated'", "'departed'", "'followed'"} {
		if !strings.Contains(query, want) {
			t.Errorf("the departure rate query does not read %s", want)
		}
	}
}
