package docs

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
)

// telemetryDoc is docs/telemetry.md itself. The Go check's container carries
// only Go sources, testdata and embedded files, so the test embeds the page.
//
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
	)
	documented(t, "attribute", telemetry.AttributeKeys()...)
	documented(t, "unit cause", recordspans.UnitCauses...)
	documented(t, "workstream cause", recordspans.CauseOwner, recordspans.CauseChief, recordspans.CauseService, recordspans.CauseAgent, recordspans.Archived, recordspans.Removed)
	documented(t, "turn outcome", recordspans.TurnCompleted, recordspans.TurnWaiting, recordspans.TurnFailed, recordspans.TurnCancelled)
	documented(t, "check kind", recordspans.CheckUnit, recordspans.CheckFinal, recordspans.CheckSession)
	documented(t, "slot outcome", slotspans.Released, slotspans.Granted, slotspans.Canceled, slotspans.Removed, slotspans.Blocked, slotspans.Abandoned, slotspans.Interrupted)
	documented(t, "wait reason", slotspans.ReasonCapacity, slotspans.ReasonWorkstreamCap)
	documented(t, "factory outcome", "reloaded", "shutdown", "replaced")
	if !strings.Contains(telemetryDoc, "semantic conventions "+telemetry.GenAIConventionsVersion) {
		t.Errorf("docs/telemetry.md does not name GenAI conventions %s", telemetry.GenAIConventionsVersion)
	}
}

// The four questions have a worked query each, against the table the
// deploy/telemetry collector writes.
func TestTelemetryPageHasTheFourQueries(t *testing.T) {
	if n := strings.Count(telemetryDoc, "```sql"); n != 4 {
		t.Fatalf("docs/telemetry.md has %d SQL queries, want 4", n)
	}
	for _, name := range []string{"osmia.pool.limit", "osmia.slot.held", "invoke_agent", "osmia.unit.state", "osmia.workstream.state"} {
		if !strings.Contains(telemetryDoc, "'"+name+"'") && !strings.Contains(telemetryDoc, "= '"+name) {
			t.Errorf("no query reads %s", name)
		}
	}
}
