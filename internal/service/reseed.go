package service

import (
	"context"
	"os"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

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

func seedTracked(ctx context.Context, clone string) (kb.Map, error) {
	dir, err := os.MkdirTemp("", "osmia-seed-")
	if err != nil {
		return kb.Map{}, err
	}
	defer os.RemoveAll(dir)
	if err := intake.CopyTracked(ctx, clone, dir); err != nil {
		return kb.Map{}, err
	}
	return kb.Seed(dir)
}
