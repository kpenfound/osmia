package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// TraceToolName is the service-run action that fetches a Dagger Cloud trace's
// output.
const TraceToolName = "read_cloud_trace"

const traceToolDescription = "Fetch the output of a Dagger Cloud trace by its trace ID, as `dagger trace <id>` shows it: the ID a failed check's `dagger trace <id>` command names. check narrows the output to one failed check by name; omit it for the whole trace. The service runs the Dagger CLI with its own Dagger Cloud token, which no role receives. The output is scrubbed of secrets and bounded: when the full output is larger than the bound, only its end is kept and truncated is true; size is the full size in bytes. The service records the output under traces/ (retrievable by all workstream roles with factory_context) and returns its path and citation; a request for the same trace ID and the same check, or the same whole trace, returns that record without fetching again, and a record for one check never answers a request for another check or the whole trace. A fetch that fails returns fetched false and a reason (no_token, token_rejected, unknown_trace, timeout, cli_unavailable or failed) and ends nothing: continue without the output."

// TraceTool is the action that fetches and records a Dagger Cloud trace's
// output for the turn scope names. Only a turn whose role is one of roles may
// call it; any other turn is refused. A failed fetch is a normal result that
// names its reason, not a tool error, so the turn continues. The recorded
// documents carry actor.
func TraceTool(f TraceFetcher, scope coreadapter.Scope, actor trace.Actor, roles ...string) coreadapter.Tool {
	return coreadapter.Tool{Name: TraceToolName, Effect: coreadapter.ToolRead, Description: traceToolDescription,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"trace_id":{"type":"string"},"check":{"type":"string"}},"required":["trace_id"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := f.Repository.ActiveTurn(scope); err != nil {
				return nil, err
			}
			if !slices.Contains(roles, scope.Role) {
				return nil, errors.New("fetching a Dagger Cloud trace is not granted")
			}
			var in struct {
				TraceID string  `json:"trace_id"`
				Check   *string `json:"check"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&in); err != nil {
				return nil, err
			}
			if err := ValidateTraceID(in.TraceID); err != nil {
				return nil, err
			}
			check := ""
			if in.Check != nil {
				if err := ValidateTraceCheck(*in.Check); err != nil {
					return nil, err
				}
				check = *in.Check
			}
			got, failure, err := f.Fetch(ctx, TraceCaller{Workstream: config.WorkstreamID(scope.Workstream), Actor: actor, Cause: scope.Turn}, in.TraceID, check)
			if err != nil {
				return nil, err
			}
			if failure != nil {
				return json.Marshal(map[string]any{"fetched": false, "trace_id": in.TraceID, "check": check, "reason": failure.Reason, "message": failure.Message})
			}
			return json.Marshal(struct {
				Fetched bool `json:"fetched"`
				TraceFetch
			}{true, got})
		}}
}
