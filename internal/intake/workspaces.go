package intake

import (
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
func Workspaces(cfg *config.Config, directory, backend string) workspace.Provider {
	dir := filepath.Join(cfg.Root.String(), directory, string(cfg.Project.ID))
	if backend == config.WorkspacesJujutsu {
		return &workspace.Jujutsu{Clone: cfg.Project.Clone, Directory: dir}
	}
	return &workspace.Git{Clone: cfg.Project.Clone, Directory: dir}
}

// streamWorkspaces are one kind of workspace of the configured project, under
// <root>/<directory>/<project>. Each workstream's are on the backend its
// trace manifest records.
type StreamWorkspaces struct {
	Cfg        *config.Config
	Repository *trace.Repository
	Directory  string
}

// of returns the provider of the workstream's workspaces.
func (w StreamWorkspaces) Of(stream config.WorkstreamID) (workspace.Provider, error) {
	backend, err := w.Repository.Workspaces(stream)
	if err != nil {
		return nil, fmt.Errorf("cannot read the workspace backend of workstream %s: %w", stream, err)
	}
	return Workspaces(w.Cfg, w.Directory, backend), nil
}
