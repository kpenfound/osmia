package service

import (
	"context"

	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type contestFixture struct {
	f      *shedFixture
	stream config.WorkstreamID
	repo   *trace.Repository
	r      *reviewers
	tool   coreadapter.Tool
}

func newContestFixture(t *testing.T, key string) *contestFixture {
	t.Helper()
	f, stream, repo := newReviewFixture(t, key)
	f.s.setSole(&activeProject{repository: repo})
	scope := intakefixture.ClaimChiefTurn(t, repo, f.project, stream, func() time.Time { return demoStart }, intakefixture.ChiefTurnInput{
		Turn: "events_1", Prompt: "Unit resume is contested.", Actor: serviceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return &contestFixture{f: f, stream: stream, repo: repo, r: &reviewers{masons: newMasonController(f.s, repo)}, tool: controls.resolveContested(repo, scope)}
}

func (c *contestFixture) state(t *testing.T) trace.WorkflowState {
	t.Helper()
	state, err := c.repo.Workflow(c.stream, trace.UnitSubject("resume"))
	must(t, err)
	return state
}

func (c *contestFixture) contest(t *testing.T) trace.Transition {
	t.Helper()
	return intakefixture.MissingVerdictContest(t, c.state, func(ctx context.Context, state trace.WorkflowState) error {
		return c.r.one(ctx, c.stream, "resume", state, false)
	}, func() { captureTurn(t, c.f, c.repo, c.stream, reviewerAgent("resume"), nil) }, func() (trace.Transition, bool, error) { return unitContest(c.repo, c.stream, "resume") })
}

func (c *contestFixture) resolve(t *testing.T, input string) string {
	t.Helper()
	return intakefixture.Handle(t, c.tool, input)
}
