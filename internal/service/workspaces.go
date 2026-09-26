package service

import (
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/workspace"
)

// workspaces returns the workspace provider of the configured project whose
// workspaces live under <root>/<directory>/<project>. It is the one place the
// service chooses a version control backend.
func workspaces(cfg *config.Config, directory string) workspace.Provider {
	return &workspace.Git{Clone: cfg.Project.Clone, Directory: filepath.Join(cfg.Root.String(), directory, string(cfg.Project.ID))}
}
