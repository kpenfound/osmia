package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// TestUnitDriver supports local candidate seeding for controller fixtures.
type TestUnitDriver struct{ *masons }

func UnitsForTest(s *Service, repo *trace.Repository) *TestUnitDriver {
	var cfg *config.Config
	if s != nil {
		cfg = s.about(repo)
	}
	return &TestUnitDriver{&masons{s: s, cfg: cfg, repository: repo}}
}

type TestBuilding struct {
	State  building
	Plan   plan.Plan
	States map[string]trace.WorkflowState
}

func (d *TestUnitDriver) Read(w config.WorkstreamID) (TestBuilding, bool, error) {
	b, found, err := d.read(w)
	return TestBuilding{State: b, Plan: b.plan, States: b.states}, found, err
}
func (d *TestUnitDriver) Start(ctx context.Context, b TestBuilding, u string) (bool, bool, error) {
	return d.start(ctx, b.State, u)
}
func (d *TestUnitDriver) Finish(ctx context.Context, b TestBuilding, u string) (bool, bool, error) {
	return d.finish(ctx, b.State, u)
}
func (d *TestUnitDriver) Bundle(ctx context.Context, w config.WorkstreamID, u string) (bundle.Mason, error) {
	return d.bundle(ctx, w, u)
}

// TestLandingDriver reconciles fixture approvals without a second service.
type TestLandingDriver struct{ *foreman }

func LandingForTest(d *TestUnitDriver) *TestLandingDriver {
	return &TestLandingDriver{&foreman{masons: d.masons}}
}
func (d *TestLandingDriver) Approval(w config.WorkstreamID, u string, v int) (trace.Document, UnitReviewResult, bool, error) {
	return d.approval(w, u, v)
}
func (d *TestLandingDriver) Current(ctx context.Context, w config.WorkstreamID, in landInput, r UnitReviewResult) (string, error) {
	return d.current(ctx, w, in, r)
}

// TestReviewerDriver records exact-candidate fixture reviews.
type TestReviewerDriver struct{ *reviewers }

func ReviewsForTest(d *TestUnitDriver) *TestReviewerDriver {
	return &TestReviewerDriver{&reviewers{masons: d.masons}}
}
func (d *TestReviewerDriver) Prepare(ctx context.Context, w config.WorkstreamID, u string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return d.prepareUnitReview(ctx, w, u)
}
func (d *TestReviewerDriver) One(ctx context.Context, w config.WorkstreamID, u string, state trace.WorkflowState, paused bool) error {
	return d.one(ctx, w, u, state, paused)
}

// TestCheckDriver reconciles checks of seeded local candidates.
type TestCheckDriver struct{ *checkers }

func ChecksForTest(d *TestUnitDriver) *TestCheckDriver {
	return &TestCheckDriver{&checkers{masons: d.masons}}
}

// TestFinalDriver prepares final-review evidence used by amendment fixtures.
type TestFinalDriver struct{ *finalReviewer }

func FinalForTest(s *Service, r *trace.Repository) *TestFinalDriver {
	return &TestFinalDriver{&finalReviewer{s: s, repository: r}}
}
func (d *TestFinalDriver) Gate(ctx context.Context, w config.WorkstreamID) (FinalReport, string, error) {
	return d.finalGate(ctx, w)
}
func (d *TestFinalDriver) Governing(ctx context.Context, w config.WorkstreamID) (finalReviewInput, seal.Seal, error) {
	return d.governing(ctx, w)
}

// RefusePublicationForTest records a local publication refusal for inbox fixtures.
func RefusePublicationForTest(ctx context.Context, s *Service, r *trace.Repository, w config.WorkstreamID, in publishInput, reason string) (coreadapter.OperationResult, error) {
	return (&publisher{s: s, repository: r}).refuse(ctx, w, in, reason)
}

// TestUnitWorkspaces exposes only opening and snapshotting fixture workspaces.
type TestUnitWorkspaces struct{ unitWorkspaces }

func UnitWorkspacesForTest(c *config.Config, r *trace.Repository) TestUnitWorkspaces {
	return TestUnitWorkspaces{newUnitWorkspaces(c, r)}
}
func (d TestUnitWorkspaces) Open(ctx context.Context, w config.WorkstreamID, u string) (workspace.Worktree, string, error) {
	return d.open(ctx, w, u)
}
func (d TestUnitWorkspaces) Snapshot(ctx context.Context, w config.WorkstreamID, u string) (workspace.Worktree, string, string, error) {
	return d.snapshot(ctx, w, u)
}

type TestFinalInput = finalReviewInput

func (d *TestUnitDriver) CandidateEvidence(ctx context.Context, w config.WorkstreamID, u string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return d.candidateEvidence(ctx, w, u)
}
