package service

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

// memoryTurns adds authenticated context tools to the existing role boundary.
// Narrowed turns retain their narrower grants, and classifiers receive none.
func memoryTurns(turns *isolation.Turns, cfg *config.Config, repo *trace.Repository) *isolation.Turns {
	turns.Context = func(ctx context.Context, scope coreadapter.Scope) (string, error) {
		return assignmentContext(repo, scope)
	}
	turns.Audit = func(ctx context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage) (func(context.Context, json.RawMessage, error) error, error) {
		return repo.BeginTool(ctx, scope, tool, raw, func() time.Time { return time.Now().UTC() })
	}

	if cfg.Hearsay.URL == "" || cfg.Project.HearsayScope == "" {
		return turns
	}
	client := hearsay.Client{Config: cfg.Hearsay}
	for role, grant := range turns.Grants {
		if role == "classifier" {
			continue
		}
		grant.Tools = slices.Clone(grant.Tools)
		for _, tool := range client.Tools(role, cfg.Project.HearsayScope, nil) {
			grant.Tools = append(grant.Tools, tool.Name)
		}
		turns.Grants[role] = grant
	}
	scoped := turns.Scoped
	turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
		var tools []coreadapter.Tool
		var err error
		if scoped != nil {
			tools, err = scoped(ctx, scope)
			if err != nil {
				return nil, err
			}
		}
		if scope.Role == "classifier" {
			return tools, nil
		}
		return append(tools, client.Tools(scope.Role, cfg.Project.HearsayScope, func() error { return repo.ActiveTurn(scope) })...), nil
	}
	return turns
}
