package service

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectmigration"
)

// loadForStart loads the configuration a starting service runs. Once the
// project import is complete the projects are the registry's, each loaded from
// the store, and no project file is read. Before it, the journalled project
// split finishes and the preflight checks that every listed project's file
// loads, with nothing else modified, and the projects load from their files.
// imported reports which of the two applied.
func loadForStart(ctx context.Context, options config.Options, step func(string) error) (cfg *config.Config, imported bool, err error) {
	top, err := config.LoadTopLevel(options)
	if err != nil {
		return nil, false, err
	}
	pipeline := projectmigration.New(top, options.Home, step)
	if imported, err = pipeline.Imported(); err != nil {
		return nil, false, err
	}
	if imported {
		cfg, err = projectmigration.Apply(top, options.Home)
		return cfg, true, err
	}
	if err := pipeline.Prepare(ctx); err != nil {
		return nil, false, err
	}
	cfg, err = config.Load(options)
	return cfg, false, err
}

// completeMigration runs what loadForStart leaves of the migration: the split
// of the listed projects and the import. The top-level file is read again,
// since finishing a registration may have listed another project. It returns
// the configuration with the projects loaded from the store.
func completeMigration(ctx context.Context, options config.Options, step func(string) error) (*config.Config, error) {
	top, err := config.LoadTopLevel(options)
	if err != nil {
		return nil, err
	}
	if err := projectmigration.New(top, options.Home, step).Complete(ctx); err != nil {
		return nil, fmt.Errorf("migrate projects: %w", err)
	}
	return projectmigration.Apply(top, options.Home)
}
