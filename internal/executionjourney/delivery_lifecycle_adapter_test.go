package executionjourney

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

type baseRefresher struct {
	s          *Service
	repository *trace.Repository
}

func (b *baseRefresher) driver() coreadapter.Reconciler {
	service.InspectForTest(b.s.core()).SetStoppedPullRequests(b.s.options.PullRequests)
	return service.BaseRefreshForTest(b.s.core(), b.repository, b.s.options.Reconciliation.Now, b.s.boundary)
}
func (b *baseRefresher) Pass(ctx context.Context) error {
	return b.driver().(interface{ Pass(context.Context) error }).Pass(ctx)
}
func (b *baseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return b.driver().Apply(ctx, op)
}
func conversationFixture(t *testing.T, key string) (Options, *config.Config) {
	o, cfg := servicetest.ConversationFixture(t, key)
	return Options{Options: o}, cfg
}
func runtimeFor(r *trace.Repository) *activeProject {
	return &activeProject{id: r.Project(), repository: r}
}

const quiet = fixturekit.Quiet
const baseRefreshAction = service.TestBaseRefreshAction
const baseObservationDocument = service.TestBaseObservationDocument

type baseObservation = service.TestBaseObservation

var baseObservationAt = service.TestBaseObservationAt
var resolved = service.TestResolved

func (s *Service) resolveBase(ctx context.Context, cfg *config.Config, r *trace.Repository, w config.WorkstreamID, p workspace.Provider) (seal.Base, error) {
	return s.intake().ResolveBase(ctx, cfg, r, w, p)
}
func resolutionDocument(t *testing.T, r *trace.Repository, w config.WorkstreamID, id, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	must(t, err)
	rev, err := intake.NextRevision(r, w, id)
	must(t, err)
	must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: rev, Project: r.Project(), Workstream: w, At: demoStart, Actor: ownerActor, Cause: "fixture"}, Path: path, Content: string(data)}}))
}

var actionOperation = fixturekit.ActionOperation

var dependentBaseChanged = service.TestDependentBaseChanged
var loadTraceWalk = service.LoadTraceWalkForTest

const driftPath = service.TestDriftPath
const driftReplayed = service.TestDriftReplayed
const publishUpstreamAction = "publish-upstream"
const deliveryReviewAction = "delivery-review"

func (b *baseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return b.driver().Inspect(ctx, op)
}

func (s *Service) holding(r *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	service.InspectForTest(s.core()).SetStoppedPullRequests(s.options.PullRequests)
	return service.InspectForTest(s.core()).Holding(r)
}
