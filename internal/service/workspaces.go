package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// workspaces returns the provider of the backend, config.WorkspacesGit or
// config.WorkspacesJujutsu, for the configured project's workspaces under
// <root>/<directory>/<project>. It is the one place the service builds a
// version control backend.
func workspaces(cfg *config.Config, directory, backend string) workspace.Provider {
	dir := filepath.Join(cfg.Root.String(), directory, string(cfg.Project.ID))
	if backend == config.WorkspacesJujutsu {
		return &workspace.Jujutsu{Clone: cfg.Project.Clone, Directory: dir}
	}
	return &workspace.Git{Clone: cfg.Project.Clone, Directory: dir}
}

// streamWorkspaces are one kind of workspace of the configured project, under
// <root>/<directory>/<project>. Each workstream's are on the backend its
// trace manifest records.
type streamWorkspaces struct {
	cfg        *config.Config
	repository *trace.Repository
	directory  string
}

// of returns the provider of the workstream's workspaces.
func (w streamWorkspaces) of(stream config.WorkstreamID) (workspace.Provider, error) {
	backend, err := w.repository.Workspaces(stream)
	if err != nil {
		return nil, fmt.Errorf("cannot read the workspace backend of workstream %s: %w", stream, err)
	}
	return workspaces(w.cfg, w.directory, backend), nil
}

// jjCheck is a check of the installed jj: the version found, and an error
// when it is missing or unsupported.
type jjCheck func(context.Context) (string, error)

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
