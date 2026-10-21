package intakefixture

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/trace"
)

// BuildFixture binds build seeding and service lifecycle operations.
type BuildFixture struct {
	Upstream func(*testing.T) string
	Seed     func(*testing.T, string, string, string, string) (config.WorkstreamID, *trace.Repository)
	Start    func(*testing.T)
}

func (f BuildFixture) SeedGit(t *testing.T, key string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	base := f.Upstream(t)
	return f.Seed(t, key, ValidPlan, config.WorkspacesGit, base)
}

// Amendment starts a building workstream on seal 1 for amendment scenarios.
func (f BuildFixture) Amendment(t *testing.T) config.WorkstreamID {
	t.Helper()
	stream, repository := f.SeedGit(t, "amendment")
	Must(t, repository.Close())
	f.Start(t)
	return stream
}

// LandingApproval identifies the candidate whose current approval is checked.
type LandingApproval struct {
	Unit            string
	Review          int
	Candidate, Base string
}

func LandingReason[R any](t *testing.T, stream config.WorkstreamID, unit string,
	approval func(config.WorkstreamID, string, int) (trace.Document, R, bool, error),
	identity func(R) reviewtools.UnitReviewIdentity,
	current func(context.Context, config.WorkstreamID, LandingApproval, R) (string, error),
) string {
	t.Helper()
	review, result := ApprovedReview(t, stream, unit, approval)
	candidate := identity(result).Candidate
	in := LandingApproval{Unit: unit, Review: review.Revision, Candidate: candidate.Revision, Base: candidate.BaseRevision}
	reason, err := current(context.Background(), stream, in, result)
	Must(t, err)
	return reason
}
