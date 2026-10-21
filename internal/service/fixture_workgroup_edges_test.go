package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// newCrossRepoEdgeDependent builds a dependent workstream whose build,
// seal and first clean final review are exactly deliveryFixtureWith's
// (one project, service stopped afterward so nothing but the test itself
// drives any pass), then registers a second project directly on the same,
// stopped service for the upstream workgroup member and records a
// cross-repo workgroup whose ratified project spec carries a merge_order
// edge from upstream to the dependent. It records no merge: callers decide
// when, mirroring the two different moments the two tests below need it.
func newCrossRepoEdgeDependent(t *testing.T) (f *shedFixture, stream config.WorkstreamID, repository *trace.Repository, report FinalReport, repo2 *trace.Repository, upstream config.WorkstreamID) {
	t.Helper()
	f, stream, repository, report = deliveryFixtureWith(t, "")
	repo2, upstream = fixturekit.CrossRepoEdgeDependent(t, f.s.cfg, f.clone, repository, stream, f.s.now, ownerActor, registrationActor, featureBranch, func(p config.Project, repo *trace.Repository) {
		f.s.setSole(&activeProject{id: repository.Project(), repository: repository})
		f.s.mu.Lock()
		f.s.cfg.Projects = append(f.s.cfg.Projects, p)
		f.s.projects = append(f.s.projects, &activeProject{id: p.ID, repository: repo})
		f.s.mu.Unlock()
	}, f.s.DraftWorkgroup)
	return f, stream, repository, report, repo2, upstream
}

// recordUpstreamOwnPullRequest records, directly on upstream's own trace,
// that its feature branch is delivered through an open, unmerged pull
// request: the identity a dependent's draft links while the merge-order
// edge stays unresolved (spec#12).
func recordUpstreamOwnPullRequest(t *testing.T, s *Service, repo2 *trace.Repository, upstream config.WorkstreamID, publication DeliveryPublication) {
	t.Helper()
	fixturekit.UpstreamOwnPullRequest(t, repo2, upstream, publication, s.now(), foremanActor, publicationDocument, publicationPath)
}
