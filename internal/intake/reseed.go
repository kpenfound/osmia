package intake

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// ReseedEntities refreshes the project's upstream default-branch ref in its
// clone with the normal upstream fetch and hands the fetched head to the
// project's re-seeder, which regenerates the entity map from it. The head is
// the project's, never a workstream's base. A failure to fetch or to re-seed
// is reported once per distinct reason and never returned: the caller goes on
// with the latest recorded map.
func (c *Controller) ReseedEntities(ctx context.Context, repo *trace.Repository) {
	if c.Reseeder == nil {
		return
	}
	reseeder := c.Reseeder(repo)
	cfg := c.About(repo)
	if reseeder == nil || !cfg.HasProject() || cfg.Project.ID != repo.Project() {
		return
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(cfg.Project.ID)})
	git := &workspace.Git{Clone: cfg.Project.Clone}
	remote, err := git.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		reseeder.Warn(fmt.Sprintf("entity map re-seed skipped: %v", err))
		return
	}
	head, err := git.Fetch(ctx, remote, cfg.Project.BaseBranch)
	if err != nil {
		reseeder.Warn(fmt.Sprintf("entity map re-seed skipped: fetching %s/%s: %v", remote, cfg.Project.BaseBranch, err))
		return
	}
	// The re-seeder reports its own failure.
	_ = reseeder.Reseed(ctx, head, git, kb.TraceLedger{Repository: repo, Actor: kb.ReseedActor, Now: c.Now})
}
