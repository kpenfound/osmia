package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verifytool"
)

func (s *Service) verifySlot(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (func(), error) {
	return s.checkSlots.acquire(ctx, CheckSlotRun{Project: project, Workstream: stream, Verify: true})
}

func (c *runtimeControls) verifySlots(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (func(), error) {
	s := c.service.Load()
	if s == nil {
		return nil, errors.New("verify service is unavailable")
	}
	return s.verifySlot(ctx, project, stream)
}

func (c *runtimeControls) currentConfig(r *trace.Repository, fallback *config.Config) *config.Config {
	if s := c.service.Load(); s != nil {
		if cfg := s.about(r); cfg != nil && cfg.HasProject() {
			return cfg
		}
	}
	return fallback
}

func verifyOffered(cfg *config.Config, scope coreadapter.Scope) bool {
	if cfg == nil || !cfg.Project.Verification.Enabled() {
		return false
	}
	switch scope.Role {
	case trace.ChiefOfStaff:
		return true
	case reviewerRole:
		return scope.Unit != "" && scope.Thread != driftReviewerAgent
	}
	return false
}

func (c *runtimeControls) verify(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, view isolation.View, now func() time.Time) coreadapter.Tool {
	env := verifytool.Env{Config: c.currentConfig(repo, cfg), Repo: repo, Scope: scope, View: view, Now: now, Slots: c.verifySlots}
	if scope.Role == trace.ChiefOfStaff {
		env.Resolve = func(ctx context.Context, in verifytool.Input) (verifytool.Target, string, error) {
			return chiefVerifyTarget(ctx, env.Config, repo, scope, in)
		}
	} else {
		env.About = "the exact candidate you are reviewing"
		env.Resolve = func(ctx context.Context, _ verifytool.Input) (verifytool.Target, string, error) {
			identity, err := execution.UnitReviewerIdentity(repo, scope)
			if err != nil {
				return verifytool.Target{}, "", err
			}
			stream := config.WorkstreamID(scope.Workstream)
			provider, err := execution.NewUnitWorkspaces(env.Config, repo).Of(stream)
			if err != nil {
				return verifytool.Target{}, "", err
			}
			return verifytool.Target{Repo: repo, Stream: stream, Config: env.Config, Unit: scope.Unit, Commit: identity.Candidate.Revision, Export: provider.ExportValidated}, "", nil
		}
	}
	return verifytool.Tool(env)
}

func chiefVerifyTarget(ctx context.Context, cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, in verifytool.Input) (verifytool.Target, string, error) {
	target, stream, refusal, err := chiefMember(repo, scope, in.Workstream)
	if err != nil || refusal != "" {
		return verifytool.Target{}, refusal, err
	}
	project := cfg.For(target.Project())
	g, err := featureWorkspaces(project, target).Of(stream)
	if err != nil {
		return verifytool.Target{}, "nothing is built yet: " + err.Error(), nil
	}
	commit, exists, err := g.Branch(ctx, featureBranch(stream))
	if err != nil {
		return verifytool.Target{}, "", err
	}
	if in.Unit != "" {
		reportDoc, err := latestPreviewDocument(target, stream, reportDocument(in.Unit))
		if err != nil {
			return verifytool.Target{}, "", err
		}
		var report UnitReport
		if reportDoc.ID != "" {
			err = json.Unmarshal([]byte(reportDoc.Content), &report)
		}
		if err != nil || report.Candidate == "" {
			return verifytool.Target{}, "unit " + in.Unit + " has no recorded candidate or cannot be resolved", nil
		}
		commit, exists = report.Candidate, true
	} else if exists {
		if sealed, _, found, err := seal.Latest(target, stream); err == nil && found && sealed.Base.Commit == commit {
			return verifytool.Target{}, "nothing is built yet on the feature branch", nil
		}
	}
	if !exists || commit == "" {
		return verifytool.Target{}, "nothing is built yet on the feature branch", nil
	}
	return verifytool.Target{Repo: target, Stream: stream, Config: project, Unit: in.Unit, Commit: commit, Export: g.ExportValidated}, "", nil
}

func (c *runtimeControls) seedVerification(ctx context.Context, cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, view isolation.View, now func() time.Time) error {
	cfg = c.currentConfig(repo, cfg)
	if !cfg.Project.Verification.Enabled() {
		return nil
	}
	switch {
	case verifyOffered(cfg, scope):
		if err := verifytool.Restore(ctx, repo, scope, view, now); err != nil {
			return err
		}
		if scope.Role == reviewerRole {
			identity, err := execution.UnitReviewerIdentity(repo, scope)
			if err != nil {
				return err
			}
			return verifytool.PlaceReruns(repo, config.WorkstreamID(scope.Workstream), scope.Unit, identity.Candidate.Revision, view)
		}
		return nil
	case scope.Role == masonRole && scope.Unit != "" && scope.Thread != driftMasonAgent:
		return execution.PlaceReviewerVerification(repo, config.WorkstreamID(scope.Workstream), scope.Unit, view)
	}
	return nil
}
