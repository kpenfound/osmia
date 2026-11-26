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
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/trace"
)

// factoryInstructions is the operating policy of a turn's role and workstream
// kind. Tests replace it to stand for a deploy that changed the policy.
var factoryInstructions = FactoryInstructionsFor

// policyKind is the registered kind of the workstream a turn belongs to. A
// scope with no workstream, and a stored kind this build does not register,
// read the feature policy: an unregistered kind runs no turns.
func policyKind(repo *trace.Repository, scope coreadapter.Scope) (kindreg.Kind, error) {
	feature, _ := kindreg.Lookup(kindreg.Feature)
	if scope.Workstream == "" || !hasPolicy(scope.Role) {
		return feature, nil
	}
	kind, _, ok, err := intake.WorkstreamKind(repo, config.WorkstreamID(scope.Workstream))
	if err != nil {
		return kindreg.Kind{}, err
	}
	if !ok {
		return feature, nil
	}
	return kind, nil
}

func memoryTurns(turns *isolation.Turns, cfg *config.Config, repo *trace.Repository) *isolation.Turns {
	turns.Context = func(ctx context.Context, scope coreadapter.Scope) (string, error) {
		kind, err := policyKind(repo, scope)
		if err != nil {
			return "", err
		}
		assignment, err := assignmentContext(repo, scope)
		return factoryInstructions(scope, kind) + assignment, err
	}
	turns.Audit = func(ctx context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage) (func(context.Context, json.RawMessage, error) error, error) {
		return repo.BeginTool(ctx, scope, tool, raw, func() time.Time { return time.Now().UTC() })
	}

	client := hearsay.Client{Config: cfg.Hearsay}
	defaultScope := cfg.HearsayScope()
	external := cfg.Hearsay.URL != "" && defaultScope != ""
	for role, grant := range turns.Grants {
		grant.Tools = slices.Clone(grant.Tools)
		if role != sessionRole && !slices.Contains(grant.Tools, ProjectContextTool) {
			grant.Tools = append(grant.Tools, ProjectContextTool)
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
		if scope.Role != sessionRole && !slices.ContainsFunc(tools, func(tool coreadapter.Tool) bool { return tool.Name == ProjectContextTool }) {
			tools = append(tools, ProjectContext(repo, scope))
		}
		if external {
			tools = append(tools, client.Tools(scope.Role, defaultScope, func() error { return repo.ActiveTurn(scope) })...)
		}
		return tools, nil
	}
	return turns
}

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
