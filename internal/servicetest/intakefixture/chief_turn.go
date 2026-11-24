package intakefixture

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// ChiefTurnInput supplies the request and claim identity of a test turn.
type ChiefTurnInput struct {
	Turn, Prompt, Cause, Token, Directory string
	Actor                                 trace.Actor
}

// ClaimChiefTurn ensures the chief thread, queues its request and claims it.
// Each trace operation receives a separate clock reading.
func ClaimChiefTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, now func() time.Time, in ChiefTurnInput) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureChiefOfStaff(ctx, stream, now(), fixturekit.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + in.Turn, Revision: 1, Project: project, Workstream: stream, At: now(), Actor: in.Actor, Cause: in.Cause},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: in.Turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: in.Prompt})
	fixturekit.Must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, in.Token, filepath.Join(t.TempDir(), in.Directory), now())
	fixturekit.Must(t, err)
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: in.Turn}
}

// ClaimContestChiefTurn claims the chief-of-staff event turn that rules on a
// contested unit.
func ClaimContestChiefTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID) coreadapter.Scope {
	t.Helper()
	return ClaimChiefTurn(t, repository, project, stream, func() time.Time { return fixturekit.DemoStart }, ChiefTurnInput{
		Turn: "events_1", Prompt: "Unit resume is contested.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
}

// ChiefEventScope is the scope of the chief-of-staff event turn that
// ClaimContestChiefTurn claims.
func ChiefEventScope(project config.ProjectID, stream config.WorkstreamID) coreadapter.Scope {
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
}

// DecideContest records the owner's fallback ruling on the unit's contest
// when the unit is contested and no ruling is recorded yet.
func DecideContest(repository *trace.Repository, stream config.WorkstreamID, unit string, unitContest func(*trace.Repository, config.WorkstreamID, string) (trace.Transition, bool, error)) error {
	contest, contested, err := unitContest(repository, stream, unit)
	if err != nil || !contested {
		return err
	}
	key := jev.ContestKey(contest.ID)
	res, err := jev.Recorded(repository, stream, jev.KindContestedRuling, key)
	if err != nil || res.Status != jev.None {
		return err
	}
	jevfixture.Fallback(repository, repository.Project(), stream, jev.KindContestedRuling, key)
	return nil
}

// ClaimHeldDriftTurn claims the chief's turn that hands back a drift rebase held for the owner.
func ClaimHeldDriftTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, now func() time.Time, id string) coreadapter.Scope {
	t.Helper()
	return ClaimChiefTurn(t, repository, project, stream, now, ChiefTurnInput{
		Turn: id, Prompt: "The drift rebase is held.", Actor: fixturekit.ServiceActor, Cause: "fixture", Token: "token-" + id, Directory: id,
	})
}
