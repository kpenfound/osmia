package telemetry

import (
	"strings"
	"testing"
)

func TestGenAIConventionsArePinnedToOneVersion(t *testing.T) {
	if !strings.HasSuffix(GenAISchemaURL, "/"+GenAIConventionsVersion) {
		t.Fatalf("schema URL %s does not match version %s", GenAISchemaURL, GenAIConventionsVersion)
	}
	for key, want := range map[string]string{
		KeyGenAIOperation: "gen_ai.operation.name", KeyGenAIProvider: "gen_ai.provider.name", KeyGenAIModel: "gen_ai.request.model",
		KeyGenAIAgentName: "gen_ai.agent.name", KeyGenAIAgentID: "gen_ai.agent.id",
	} {
		if key != want {
			t.Errorf("key %q, want %q", key, want)
		}
	}
	if GenAIOperationInvokeAgent != "invoke_agent" {
		t.Fatalf("operation %q", GenAIOperationInvokeAgent)
	}
	if got := GenAIName(GenAIOperationInvokeAgent, "mason").String(); got != "invoke_agent mason" {
		t.Fatalf("span name %q", got)
	}
	for name, a := range map[string]Attr{"agent name": GenAIAgentName("mason"), "agent id": GenAIAgentID("mason-1"), "operation": GenAIOperation(GenAIOperationInvokeAgent)} {
		if !a.Valid() {
			t.Errorf("%s attribute is dropped", name)
		}
	}
	if GenAIAgentName("two words").Valid() || GenAIAgentID("with\nline").Valid() {
		t.Error("free text became an agent attribute")
	}
}
