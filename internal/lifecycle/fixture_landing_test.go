package lifecycle_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func landOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.LandOperations(t, repository, stream, LandAction)
}
func approvedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, UnitReviewResult) {
	t.Helper()
	f := &foreman{masons: &masons{repository: repository}}
	return intakefixture.ApprovedReview(t, stream, unit, f.approval)
}
func (f *shedFixture) awaitMerged(t *testing.T, stream config.WorkstreamID, unit string) {
	t.Helper()
	intakefixture.AwaitMerged(t, f.sharedTurns(), stream, unit, func(t *testing.T) []trace.OperationRecord { return landOperations(t, f.repository(), stream) })
}
func approveDirectly(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, unit, criterion string) {
	t.Helper()
	r := &reviewers{masons: newMasonController(s, repository)}
	intakefixture.ApproveDirectly(t, repository, stream, unit, r.prepareUnitReview, func(identity UnitReviewIdentity, turn string) ([]byte, error) {
		return json.Marshal(UnitReviewResult{Identity: identity, Turn: turn, Verdict: UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}})
	}, reviewTurnID, reviewDocument, r.one, UnitApproved)
}
func newApprovedFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return newApprovedFixtureOn(t, key, config.WorkspacesGit)
}
func newApprovedFixtureOn(t *testing.T, key, backend string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixtureOn(t, backend, "masons = 1\n", independentPlan, "")
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, backend, base)
	intakefixture.SeedApprovedUnits(t, func(u, o string) { seedReview(t, f, repository, stream, u, o) }, func(u, c string) { approveDirectly(t, f.s, repository, stream, u, c) })
	return f, stream, repository
}
