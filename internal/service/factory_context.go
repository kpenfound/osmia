package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const factoryContextTool = "factory_context"

// factoryContext exposes workstream records independently of conversation replay.
func factoryContext(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: factoryContextTool, Effect: coreadapter.ToolRead,
		Description: "Read the workstream's durable factory records, including your current task in plan.json, spec.md, unit reports, check output, discoveries, and neighboring assignments. With no path list available documents and revisions. Set revision to read earlier evidence. Use start and lines to select text. Large excerpts return next_offset; pass it as offset with the same line range to continue. This reads factory state, not target-project files.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"offset":{"type":"integer","minimum":0},"path":{"type":"string"},"revision":{"type":"integer","minimum":1},"start":{"type":"integer","minimum":1},"lines":{"type":"integer","minimum":1,"maximum":1000}},"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := r.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Offset   int    `json:"offset"`
				Path     string `json:"path"`
				Revision int    `json:"revision"`
				Start    int    `json:"start"`
				Lines    int    `json:"lines"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			if in.Start == 0 {
				in.Start = 1
			}
			if in.Lines == 0 {
				in.Lines = 200
			}
			if in.Offset < 0 || in.Start < 1 || in.Lines < 1 || in.Lines > 1000 {
				return nil, errors.New("invalid range")
			}
			docs, err := trace.Read[trace.Document](r, config.WorkstreamID(scope.Workstream))
			if err != nil {
				return nil, err
			}
			latest := map[string]int{}
			var selected trace.Document
			for _, d := range docs {
				if strings.HasPrefix(d.Path, "tools/") || strings.HasPrefix(d.Path, "inspections/") {
					continue
				}
				latest[d.Path] = max(latest[d.Path], d.Revision)
				if d.Path == in.Path && (in.Revision == 0 || in.Revision == d.Revision) && d.Revision >= selected.Revision {
					selected = d
				}
			}
			if in.Path == "" {
				return json.Marshal(latest)
			}
			if selected.Revision == 0 {
				return nil, errors.New("factory document revision not found")
			}
			lines := strings.Split(selected.Content, "\n")
			start := min(len(lines), in.Start-1)
			end := min(len(lines), start+in.Lines)
			content := strings.Join(lines[start:end], "\n")
			offset := min(in.Offset, len(content))
			for offset < len(content) && !utf8.RuneStart(content[offset]) {
				offset++
			}
			limit := min(len(content), offset+65536)
			for limit < len(content) && !utf8.RuneStart(content[limit]) {
				limit--
			}
			next := 0
			if limit < len(content) {
				next = limit
			}
			return json.Marshal(map[string]any{"path": selected.Path, "revision": selected.Revision, "start": in.Start, "total_lines": len(lines), "offset": offset, "next_offset": next, "content": content[offset:limit]})
		}}
}

// assignmentContext refreshes essential unit context on every turn, including recovery turns.
func assignmentContext(r *trace.Repository, scope coreadapter.Scope) (string, error) {
	if scope.Unit == "" || (scope.Role != masonRole && scope.Role != reviewerRole) {
		return "", nil
	}
	stream := config.WorkstreamID(scope.Workstream)
	current, _, found, err := seal.Latest(r, stream)
	if err != nil || !found {
		return "", err
	}
	unit, err := sealedUnit(r, scope)
	if err != nil {
		return "", err
	}
	graph, err := sealedPlan(r, stream, current.Revision.Plan)
	if err != nil {
		return "", err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(unit)
	out := fmt.Sprintf("\n\nCurrent assignment (plan revision %d; supersedes older assignment text):\n%s\nRead factory_context for spec.md, plan.json, prior reports and checks. Adapt implementation within these constraints. Coordinate before taking another unit's responsibility. Neighboring responsibilities:\n", current.Revision.Plan, data)
	for _, u := range p.Units {
		if u.ID != unit.ID {
			out += fmt.Sprintf("- %s: %s; depends on %v; boundaries %v\n", u.ID, u.Task, u.DependsOn, u.Boundaries)
		}
	}
	discovered, _, err := unitDiscoveries(r, stream, unit.ID)
	if err != nil {
		return "", err
	}
	if discovered != "" {
		out += "\nNecessary discovered work assigned to this unit (account for it in implementation and verification):\n" + discovered
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return "", err
	}
	var report trace.Document
	for _, d := range docs {
		if d.ID == reportDocument(unit.ID) && d.Revision > report.Revision {
			report = d
		}
	}
	if report.Revision > 0 {
		content := report.Content
		if len(content) > 24000 {
			content = content[:24000] + "\n[Use factory_context for the rest.]"
		}
		out += fmt.Sprintf("\nLatest report (%s revision %d):\n%s", report.Path, report.Revision, content)
	}
	return out, nil
}
