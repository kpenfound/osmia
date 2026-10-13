package service

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

func checkKnowledge(repo *trace.Repository, scope coreadapter.Scope, view *isolation.FileView) coreadapter.Tool {
	return coreadapter.Tool{Name: "kb_check", Effect: coreadapter.ToolRead,
		Description: "Validate the complete output/kb in this turn's view before finishing. Reports structural errors and prose budgets without publishing anything. Rewrite oversized or outdated prose, then check again. Limits are ceilings, not targets.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Handle: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			if err := repo.ActiveTurn(scope); err != nil {
				return nil, err
			}
			out, err := kb.ReadOutput(filepath.Join(view.Workspace().Directory, kb.OutputDirectory))
			result := map[string]any{"valid": err == nil, "max_file_bytes": kb.MaxProseBytes, "max_total_bytes": kb.MaxTotalProseBytes}
			if err != nil {
				result["problem"] = contextExcerpt(err.Error(), 16000)
			} else {
				total := 0
				for _, text := range out.Prose {
					total += len(text)
				}
				result["files"], result["total_bytes"] = len(out.Prose), total
			}
			return json.Marshal(result)
		}}
}
