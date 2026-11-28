package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/execution"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type contestFixture struct {
	f        *shedFixture
	stream   config.WorkstreamID
	repo     *trace.Repository
	r        *reviewers
	tool     coreadapter.Tool
	scope    coreadapter.Scope
	controls *runtimeControls
}

func newContestFixture(t *testing.T, key string) *contestFixture {
	t.Helper()
	f, stream, repo := newReviewFixture(t, key)
	f.s.setSole(&activeProject{repository: repo})
	scope := intakefixture.ClaimContestManagerTurn(t, repo, f.project, stream)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return &contestFixture{f: f, stream: stream, repo: repo, r: &reviewers{masons: newMasonController(f.s, repo)}, tool: controls.resolveContested(repo, scope), scope: scope, controls: controls}
}

func (c *contestFixture) contest(t *testing.T) trace.Transition {
	t.Helper()
	return intakefixture.MissingVerdictContest(t, func(t *testing.T) trace.WorkflowState {
		return intakefixture.UnitWorkflow(t, c.repo, c.stream, "resume")
	}, func(ctx context.Context, state trace.WorkflowState) error {
		return c.r.one(ctx, c.stream, "resume", state, false)
	}, func() { captureTurn(t, c.f, c.repo, c.stream, reviewerAgent("resume"), nil) }, func() (trace.Transition, bool, error) { return unitContest(c.repo, c.stream, "resume") })
}

func (c *contestFixture) resolve(t *testing.T, input string) string {
	t.Helper()
	c.decided(t)
	return intakefixture.Handle(t, c.tool, input)
}

func (c *contestFixture) decided(t *testing.T) {
	t.Helper()
	fixturekit.Must(t, intakefixture.DecideContest(c.repo, c.stream, "resume", execution.UnitContest))
}
