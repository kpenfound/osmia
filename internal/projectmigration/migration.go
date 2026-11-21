// Package projectmigration moves a root's projects from the files an earlier
// Osmia version kept, active_projects in the top-level file and one config.toml
// per project, into the project store.
//
// Until the import is complete, a run goes through these steps in order:
//
//  1. a project split an earlier run journalled in project-splits.json
//     finishes;
//  2. the preflight checks that every listed project's file loads, and stops
//     the run, with nothing modified by it, when one does not;
//  3. a project split covers the listed projects that name a group; the files
//     of unlisted projects are neither read nor rewritten;
//  4. the import records the list and each listed file in the store, retires
//     the files and records its completion.
//
// Every step resumes after an interruption. Once the import is complete a run
// reads no project file and looks for no split.
package projectmigration

import (
	"context"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

// Pipeline runs the migration steps of one root.
type Pipeline struct {
	cfg   *config.Config
	home  string
	step  func(string) error
	store *projectstore.Store
}

// New returns the pipeline of cfg's root. cfg is the top-level configuration,
// whose active_projects names the projects to import. home is the home
// directory the loader expands ~ against. step, when set, is called after each
// durable step with its name; a failure stops the run there, as a crash would.
func New(cfg *config.Config, home string, step func(string) error) *Pipeline {
	if step == nil {
		step = func(string) error { return nil }
	}
	return &Pipeline{cfg: cfg, home: home, step: step, store: projectstore.Open(cfg.Root, projectstore.WithStep(step))}
}

// Imported reports whether the import is complete.
func (p *Pipeline) Imported() (bool, error) { return p.store.Imported() }

// Prepare runs the steps that leave every project file as it was, apart from
// finishing a journalled split: the journalled split, then the preflight. It
// does nothing once the import is complete.
func (p *Pipeline) Prepare(ctx context.Context) error {
	if done, err := p.store.Imported(); err != nil || done {
		return err
	}
	if err := p.finishJournalled(ctx); err != nil {
		return err
	}
	return p.store.Preflight(p.cfg, p.home)
}

// Complete runs the remaining steps: the split of the listed projects, then
// the import. Prepare must have run on this root first. It does nothing once
// the import is complete.
func (p *Pipeline) Complete(ctx context.Context) error {
	if done, err := p.store.Imported(); err != nil || done {
		return err
	}
	if err := p.store.Preflight(p.cfg, p.home); err != nil {
		return err
	}
	if err := p.discover(ctx); err != nil {
		return err
	}
	return p.store.Import(ctx, p.cfg, p.home)
}

// Run runs every step until the import is complete.
func (p *Pipeline) Run(ctx context.Context) error {
	if err := p.Prepare(ctx); err != nil {
		return err
	}
	return p.Complete(ctx)
}

// Apply returns cfg with the registry's projects, in registry order, each
// loaded from its stored settings. cfg is the top-level configuration of a root
// whose import is complete. A leftover active_projects is ignored, with a load
// warning. cfg is unchanged.
func Apply(cfg *config.Config, home string) (*config.Config, error) {
	out, err := projectstore.Open(cfg.Root).Apply(cfg, home)
	if err != nil {
		return nil, err
	}
	if len(cfg.ActiveProjects) > 0 {
		out.Warnings = append(slices.Clone(cfg.Warnings), config.ActiveProjectsIgnored)
	}
	return out, nil
}
