package journey

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	strings "strings"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func NewReviewFixture(t *testing.T, key string) (*ShedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := SeedBuild(t, f, key, intakefixture.IndependentPlan, config.WorkspacesGit, base)
	SeedReview(t, f, repository, stream, "resume", ResumeReport)
	return f, stream, repository
}

func (f *ShedFixture) ShedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	return f.Scenario().ShedAs(t, key)
}

func (f *ShedFixture) RatifiedBuild(t *testing.T, stream config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	return f.Scenario().RatifiedBuild(t, stream)
}
