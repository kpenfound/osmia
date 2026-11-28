package telemetry

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestSpansReportTheManagerRoleByItsDisplayName(t *testing.T) {
	for name, attr := range map[string]Attr{
		"role":            Role(trace.Manager),
		"agent name":      GenAIAgentName(trace.Manager),
		"agent id":        GenAIAgentID(trace.Manager),
		"already display": Role("manager"),
	} {
		if !attr.Valid() || attr.Value().AsString() != "manager" {
			t.Errorf("%s = %q (valid %t), want manager", name, attr.Value().AsString(), attr.Valid())
		}
	}
	if got := PoolID(trace.Manager); got != "manager" {
		t.Errorf("pool of the manager role = %q, want manager", got)
	}
	if got := Role("mason").Value().AsString(); got != "mason" {
		t.Errorf("role mason = %q", got)
	}
	if got := GenAIName(GenAIOperationInvokeAgent, trace.DisplayRole(trace.Manager)).String(); got != "invoke_agent manager" {
		t.Errorf("turn span name %q", got)
	}
}
