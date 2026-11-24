package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/workspace"
)

type jjCheck func(context.Context) (string, error)

const jjCheckTimeout = 5 * time.Second

func checkJJ(ctx context.Context) (string, error) { return workspace.CheckJJ(ctx, "") }

func (s *Service) newWorkspaces(ctx context.Context, cfg *config.Config) (WorkspacesStatus, error) {
	setting := cfg.Workspaces
	if setting == "" {
		setting = config.WorkspacesAuto
	}
	status := WorkspacesStatus{Setting: setting, Backend: config.WorkspacesGit}
	if setting == config.WorkspacesGit {
		return status, nil
	}
	check := s.options.checkJJ
	if check == nil {
		check = checkJJ
	}
	version, err := check(ctx)
	status.JJ = version
	switch {
	case err == nil:
		status.Backend = config.WorkspacesJujutsu
	case setting == config.WorkspacesJujutsu:
		status.Backend = ""
		status.Problem = jjProblem(err)
		return status, errors.New(status.Problem)
	}
	return status, nil
}

func jjProblem(err error) string {
	return fmt.Sprintf("workspaces is %q but no supported jj is installed (%v); install jj %s or later, or set workspaces to %q or %q", config.WorkspacesJujutsu, err, workspace.MinimumJJ, config.WorkspacesAuto, config.WorkspacesGit)
}
