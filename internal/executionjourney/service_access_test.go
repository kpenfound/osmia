package executionjourney

import (
	"context"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
)

func newWorkspaces(s *journey.Service, ctx context.Context, c *config.Config) (api.WorkspacesStatus, error) {
	return s.Intake().NewWorkspaces(ctx, c)
}

func recordContestedRuling(s *journey.Service, ctx context.Context, r *trace.Repository, w config.WorkstreamID, u string, req api.ContestedRulingRequest, a trace.Actor, ds ...trace.Document) (api.ContestedRulingResponse, *api.APIError) {
	return s.Execution().RecordContestedRuling(ctx, r, w, u, req, a, ds...)
}

var factoryTarget = runtime.Target{Scope: "factory"}

func setReviewChecks(s *journey.Service, checker checks.ReviewChecks) {
	s.Options.ReviewChecks = checker
	journey.InspectForTest(s).SetReviewChecks(checker)
}
