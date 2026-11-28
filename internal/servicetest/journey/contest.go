package journey

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// ContestFixture holds a unit awaiting review and the manager tool
// that rules on its contest.
type ContestFixture struct {
	F      *ShedFixture
	Stream config.WorkstreamID
	Repo   *trace.Repository
	R      *Reviewers
	Tool   coreadapter.Tool
}

// NewContestFixture seeds a unit under review for key and claims the manager
// event turn that rules on its contest.
func NewContestFixture(t *testing.T, key string) *ContestFixture {
	t.Helper()
	f, stream, repo := NewReviewFixture(t, key)
	f.S.SetSole(&ActiveProject{Repository: repo})
	scope := intakefixture.ClaimContestManagerTurn(t, repo, f.Project, stream)
	controls := &RuntimeControls{}
	controls.Service.Store(f.S)
	return &ContestFixture{F: f, Stream: stream, Repo: repo, R: &Reviewers{Masons: NewMasonController(f.S, repo)}, Tool: controls.Execution().ResolveContested(repo, scope)}
}

// Contest drives the unit's review to a contest: a review turn that ends
// without a verdict.
func (c *ContestFixture) Contest(t *testing.T) trace.Transition {
	t.Helper()
	return intakefixture.MissingVerdictContest(t, func(t *testing.T) trace.WorkflowState {
		return intakefixture.UnitWorkflow(t, c.Repo, c.Stream, "resume")
	}, func(ctx context.Context, state trace.WorkflowState) error {
		return c.R.One(ctx, c.Stream, "resume", state, false)
	}, func() { CaptureTurn(t, c.F, c.Repo, c.Stream, intake.ReviewerAgent("resume"), nil) }, func() (trace.Transition, bool, error) { return execution.UnitContest(c.Repo, c.Stream, "resume") })
}

// Resolve calls the resolve_contested tool with input after the owner's
// fallback ruling on the contest is recorded.
func (c *ContestFixture) Resolve(t *testing.T, input string) string {
	t.Helper()
	fixturekit.Must(t, intakefixture.DecideContest(c.Repo, c.Stream, "resume", execution.UnitContest))
	return intakefixture.Handle(t, c.Tool, input)
}

// Raised reports whether contest was raised to the owner, and the note that
// came with it.
func (c *ContestFixture) Raised(t *testing.T, contest trace.Transition) (bool, string) {
	t.Helper()
	raised, note, err := execution.ContestRaised(c.Repo, c.Stream, "resume", contest)
	fixturekit.Must(t, err)
	return raised, note
}

// MoveFixture is a ContestFixture with the manager move_unit tool.
type MoveFixture struct {
	*ContestFixture
	Move coreadapter.Tool
}

// NewMoveFixture returns a ContestFixture for key and its move_unit tool.
func NewMoveFixture(t *testing.T, key string) *MoveFixture {
	t.Helper()
	c := NewContestFixture(t, key)
	controls := &RuntimeControls{}
	controls.Service.Store(c.F.S)
	return &MoveFixture{ContestFixture: c, Move: controls.Execution().MoveUnit(c.Repo, intakefixture.ManagerEventScope(c.F.Project, c.Stream))}
}
