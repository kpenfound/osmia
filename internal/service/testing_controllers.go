package service

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
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
	return TestBuilding{State: b, Plan: b.Plan(), States: b.States()}, found, err
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
type TestFinalDriver struct{ *delivery.FinalReviewer }

func FinalForTest(s *Service, r *trace.Repository) *TestFinalDriver {
	return &TestFinalDriver{&delivery.FinalReviewer{S: s.delivery(), Repository: r}}
}
func (d *TestFinalDriver) Gate(ctx context.Context, w config.WorkstreamID) (FinalReport, string, error) {
	return d.FinalGate(ctx, w)
}
func (d *TestFinalDriver) Governing(ctx context.Context, w config.WorkstreamID) (finalReviewInput, seal.Seal, error) {
	return d.FinalReviewer.Governing(ctx, w)
}

// RefusePublicationForTest records a local publication refusal for inbox fixtures.
func RefusePublicationForTest(ctx context.Context, s *Service, r *trace.Repository, w config.WorkstreamID, in publishInput, reason string) (coreadapter.OperationResult, error) {
	return (&delivery.Publisher{S: s.delivery(), Repository: r}).Refuse(ctx, w, in, reason)
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

// TestDriftDriver lets local execution fixtures reconcile a drift that carries units.
type TestDriftDriver struct{ drifter }

func DriftForTest(s *Service, r *trace.Repository) *TestDriftDriver {
	return &TestDriftDriver{drifter{&foreman{masons: &masons{s: s, cfg: s.about(r), repository: r}}}}
}
func (d *TestDriftDriver) Request(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return d.RequestDrifts(ctx, due)
}

// BaseRefreshForTest reconciles a local observation with fixture boundaries.
func BaseRefreshForTest(s *Service, repo *trace.Repository, now func() time.Time, boundary func(string) error) coreadapter.Reconciler {
	if now != nil {
		s.options.Reconciliation.Now = now
	}
	s.boundary = boundary
	return &baseRefresher{S: s.lifecycle(), Repository: repo}
}

type TestBaseObservation = baseObservation

var TestBaseObservationAt = baseObservationAt
var TestResolved = resolved

const TestBaseObservationDocument = baseObservationDocument

const TestDriftPath = driftPath
const TestDriftReplayed = driftReplayed

var TestDependentBaseChanged = dependentBaseChanged

// TestTraceWalk reads trace provenance used by delivery integration assertions.
type TestTraceWalk struct{ walk *traceWalk }

func LoadTraceWalkForTest(r *trace.Repository, w config.WorkstreamID) (*TestTraceWalk, error) {
	v, e := loadTraceWalk(r, w)
	return &TestTraceWalk{v}, e
}
func (w *TestTraceWalk) Delivery() (*TraceDelivery, *FinalReport, []TraceGap) {
	return w.walk.delivery()
}
