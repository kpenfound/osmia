package telemetry

import (
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// GenAIConventionsVersion is the one version of the OpenTelemetry GenAI semantic
// conventions turn spans follow. The attribute keys in attrs.go come from the
// semconv package of this version, so a different version is a change to the
// import above and nowhere else.
const GenAIConventionsVersion = "1.40.0"

// GenAISchemaURL is the schema URL of the pinned semantic conventions.
const GenAISchemaURL = semconv.SchemaURL

// GenAIOperationInvokeAgent is the gen_ai.operation.name of a turn span: a turn
// is one invocation of an agent, so its span is named "invoke_agent" and the
// agent's name, as the agent invocation span of the conventions prescribes.
var GenAIOperationInvokeAgent = semconv.GenAIOperationNameInvokeAgent.Value.AsString()
