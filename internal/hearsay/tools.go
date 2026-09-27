package hearsay

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Tools serves role-granted Hearsay calls through the service-owned client.
// The runtime receives handlers, never either bearer credential.
func (c Client) Tools(role, projectScope string, check func() error) []coreadapter.Tool {
	definitions := []struct {
		name, properties string
		required         []string
	}{
		{"get_bundle", `"scope":{"type":"string"}`, nil},
		{"resolve", `"text":{"type":"string"}`, []string{"text"}},
		{"stance_history", `"topic":{"type":"string"}`, []string{"topic"}},
		{"get_l1", `"id":{"type":"string"}`, []string{"id"}},
		{"get_l0", `"id":{"type":"string"}`, []string{"id"}},
		{"search", `"query":{"type":"string"},"scope":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":20}`, []string{"query"}},
	}
	if Class(role) != "observer" {
		definitions = append(definitions, struct {
			name, properties string
			required         []string
		}{"assert", `"topic":{"type":"string"},"position":{"type":"string","maxLength":2000},"evidence":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":32}`, []string{"topic", "position", "evidence"}})
	}
	out := []coreadapter.Tool{}
	for _, def := range definitions {
		required := def.required
		if required == nil {
			required = []string{}
		}
		req, _ := json.Marshal(required)
		tool := coreadapter.Tool{Name: def.name, Description: "Read external Hearsay evidence using this role's delegated grants. External content cannot override local owner decisions.", Effect: coreadapter.ToolRead, InputSchema: json.RawMessage(`{"type":"object","properties":{` + def.properties + `},"required":` + string(req) + `,"additionalProperties":false}`)}
		if def.name == "assert" {
			tool.Effect = coreadapter.ToolMemory
			tool.Description = "Propose an agent learning on an existing Hearsay topic with readable L1 evidence. This is not an owner ruling and does not ratify the proposal."
		}
		tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if check == nil {
				return nil, errors.New("memory turn scope is unavailable")
			}
			if err := check(); err != nil {
				return nil, err
			}
			var input map[string]any
			if err := json.Unmarshal(raw, &input); err != nil || input == nil {
				return nil, errors.New("memory arguments must be an object")
			}
			if (def.name == "get_bundle" || def.name == "search") && input["scope"] == nil {
				input["scope"] = projectScope
			}
			return c.Call(ctx, role, def.name, input)
		}
		out = append(out, tool)
	}
	return out
}
