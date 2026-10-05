package service

import (
	"context"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/status"
)

// beekeeperOwnerGateTools names every action the Beekeeper's toolset must
// not include: approving or refusing delivery, ruling on a contested unit,
// approving a spec or plan, editing or ratifying the charter, answering a
// question or opening a workstream. There is no workstream-opening tool
// anywhere in the service yet, so this list cannot name one; the exact-set
// assertion below still proves the toolset carries none.
var beekeeperOwnerGateTools = []string{
	resolveContestedTool, decideAmendmentTool, decideCharterTool, prioritiseTool,
	pauseTool, resumeTool, status.ToolName, "notify", "inspect_code", "capacity", moveUnitTool,
	questions.AskTool, questions.AmendTool, doneTool, verdictTool, workstreamDiffTool,
	questions.AnswerTool, questions.EscalateTool, questions.RelayRulingTool, questions.RouteAmendmentTool, questions.ProposeCharterTool,
}

// checkBeekeeperToolset asserts that the production wiring Enforce builds
// gives a real Beekeeper turn exactly the list tool and the message tool,
// built with a fake engine and MCP host (no live model session, per
// charter#4), and no owner-gate, question-answering or workstream-opening
// action.
func checkBeekeeperToolset(t *testing.T, opts Options) {
	t.Helper()
	enforced := Enforce(opts, Enforcement{})
	bt, ok := enforced.BeekeeperTurns.(*isolation.Turns)
	if !ok {
		t.Fatalf("beekeeper turns runner is %T, not *isolation.Turns", enforced.BeekeeperTurns)
	}
	want := []string{listFactoryTool, messageChiefOfStaffTool}
	sortedWant := slices.Clone(want)
	slices.Sort(sortedWant)

	grant, ok := bt.Grants[beekeeper.AgentID]
	if !ok {
		t.Fatal("the beekeeper role has no service grant")
	}
	got := slices.Clone(grant.Tools)
	slices.Sort(got)
	if !slices.Equal(got, sortedWant) {
		t.Fatalf("beekeeper grant tools: %v, want %v", got, sortedWant)
	}

	scope := coreadapter.Scope{Project: string(config.ShadowProjectID), Workstream: string(config.BeekeeperWorkstreamID), Thread: beekeeper.ThreadID, Turn: "turn_1", Role: beekeeper.AgentID}
	tools, err := bt.Scoped(context.Background(), scope)
	must(t, err)
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	slices.Sort(names)
	if !slices.Equal(names, sortedWant) {
		t.Fatalf("beekeeper runtime session tools: %v, want exactly %v", names, sortedWant)
	}
	for _, forbidden := range beekeeperOwnerGateTools {
		if slices.Contains(names, forbidden) {
			t.Fatalf("beekeeper toolset includes the owner-gate or escalation action %q", forbidden)
		}
	}

	// A scope outside the Beekeeper's own role is refused by both Select
	// and Scoped: no other role reaches these tools through this wiring.
	if _, err := bt.Select(context.Background(), coreadapter.Scope{Role: "chief_of_staff"}); err == nil {
		t.Fatal("beekeeper view selection accepted a chief-of-staff scope")
	}
	if _, err := bt.Scoped(context.Background(), coreadapter.Scope{Role: "chief_of_staff"}); err == nil {
		t.Fatal("beekeeper tool scoping accepted a chief-of-staff scope")
	}
}

// The Beekeeper's runtime session gets exactly the list tool and the
// message tool from the production wiring Enforce builds, and nothing a
// real Beekeeper turn runs through Enforce could use to approve or refuse
// delivery, rule on a contested unit, approve a spec or plan, edit or
// ratify the charter, answer a question or open a workstream.
func TestBeekeeperRuntimeToolsetIsExactlyListAndMessage(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	checkBeekeeperToolset(t, opts)
}

// The same toolset is built with Hearsay not configured: the Beekeeper's
// tools come only from its dedicated wiring, never from the Hearsay memory
// tools every other role gets when Hearsay is configured, so its toolset
// does not depend on whether Hearsay is configured at all.
func TestBeekeeperRuntimeToolsetWithHearsayNotConfigured(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	if cfg.Hearsay.URL != "" || len(cfg.Hearsay.Agents) != 0 {
		t.Fatalf("test assumes Hearsay is not configured: %+v", cfg.Hearsay)
	}
	checkBeekeeperToolset(t, opts)
}
