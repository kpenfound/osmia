package service

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/trace"
)

// memoryTurns adds authenticated context tools to the existing role boundary.
// Narrowed turns retain their narrower grants, and classifiers receive none.
// Exploratory sessions receive only the read tools, at the observer class.
func memoryTurns(turns *isolation.Turns, cfg *config.Config, repo *trace.Repository) *isolation.Turns {
	turns.Context = func(ctx context.Context, scope coreadapter.Scope) (string, error) {
		assignment, err := assignmentContext(repo, scope)
		return factoryInstructions(scope) + assignment, err
	}
	turns.Audit = func(ctx context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage) (func(context.Context, json.RawMessage, error) error, error) {
		return repo.BeginTool(ctx, scope, tool, raw, func() time.Time { return time.Now().UTC() })
	}

	client := hearsay.Client{Config: cfg.Hearsay}
	defaultScope := cfg.HearsayScope()
	external := cfg.Hearsay.URL != "" && defaultScope != ""
	for role, grant := range turns.Grants {
		if role == "classifier" {
			continue
		}
		grant.Tools = slices.Clone(grant.Tools)
		if role != sessionRole && !slices.Contains(grant.Tools, projectContextTool) {
			grant.Tools = append(grant.Tools, projectContextTool)
		}
		if external {
			for _, tool := range client.Tools(role, defaultScope, nil) {
				if !slices.Contains(grant.Tools, tool.Name) {
					grant.Tools = append(grant.Tools, tool.Name)
				}
			}
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
		if scope.Role != sessionRole && !slices.ContainsFunc(tools, func(tool coreadapter.Tool) bool { return tool.Name == projectContextTool }) {
			tools = append(tools, projectContext(repo, scope))
		}
		if external {
			tools = append(tools, client.Tools(scope.Role, defaultScope, func() error { return repo.ActiveTurn(scope) })...)
		}
		return tools, nil
	}
	return turns
}

// registerHearsaySecrets registers the credential values the configuration
// references, so they are redacted from logs and errors from the moment the
// configuration is loaded.
func registerHearsaySecrets(h config.Hearsay) {
	if h.URL == "" {
		return
	}
	redact.Register(os.Getenv(h.TokenEnv))
	for _, agent := range h.Agents {
		redact.Register(os.Getenv(agent.TokenEnv))
	}
	hearsay.PublisherToken(h, nil)
}
