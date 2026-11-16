package service

import (
	"context"
	"os"

	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// reseederOf returns the re-seeder of the entity map of the project repo is
// the trace of. Every trigger of the project shares it, so each upstream
// commit is seeded once.
func (s *Service) reseederOf(repo *trace.Repository) *kb.Reseeder {
	if repo == nil {
		return nil
	}
	project := repo.Project()
	if r, ok := s.reseeders.Load(project); ok {
		return r.(*kb.Reseeder)
	}
	r, _ := s.reseeders.LoadOrStore(project, &kb.Reseeder{Log: func(reason string) {
		s.log().Warn("entity map not re-seeded", "project", string(project), "reason", reason)
	}})
	return r.(*kb.Reseeder)
}

// seedTracked seeds the entity map a registration records from the clone's tracked
// files, through the seed every re-seed uses.
func seedTracked(ctx context.Context, clone string) (kb.Map, error) {
	dir, err := os.MkdirTemp("", "osmia-seed-")
	if err != nil {
		return kb.Map{}, err
	}
	defer os.RemoveAll(dir)
	if err := copyTracked(ctx, clone, dir); err != nil {
		return kb.Map{}, err
	}
	return kb.Seed(dir)
}
