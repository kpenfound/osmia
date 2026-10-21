package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"

	"github.com/kpenfound/osmia/internal/workspace"
)

// jjCheck is a check of the installed jj: the version found, and an error
// when it is missing or unsupported.
type jjCheck func(context.Context) (string, error)

// jjCheckTimeout bounds the check of jj that a status read runs.
const jjCheckTimeout = 5 * time.Second

// checkJJ checks the jj on the service's PATH.
func checkJJ(ctx context.Context) (string, error) { return workspace.CheckJJ(ctx, "") }

// newWorkspaces reports the backend a workstream created now under cfg gets.
// The error of workspaces = "jujutsu" without a supported jj names the
// problem and the version found.
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

// jjProblem says why workspaces = "jujutsu" cannot create a workstream.
func jjProblem(err error) string {
	return fmt.Sprintf("workspaces is %q but no supported jj is installed (%v); install jj %s or later, or set workspaces to %q or %q", config.WorkspacesJujutsu, err, workspace.MinimumJJ, config.WorkspacesAuto, config.WorkspacesGit)
}
