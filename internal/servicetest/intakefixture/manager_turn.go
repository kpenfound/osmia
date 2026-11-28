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

// ManagerTurnInput supplies the request and claim identity of a test turn.
type ManagerTurnInput struct {
	Turn, Prompt, Cause, Token, Directory string
	Actor                                 trace.Actor
}

// ClaimManagerTurn ensures the manager thread, queues its request and claims it.
// Each trace operation receives a separate clock reading.
func ClaimManagerTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, now func() time.Time, in ManagerTurnInput) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureManager(ctx, stream, now(), fixturekit.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + in.Turn, Revision: 1, Project: project, Workstream: stream, At: now(), Actor: in.Actor, Cause: in.Cause},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: in.Turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "manager", Prompt: in.Prompt})
	fixturekit.Must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, trace.Manager, in.Token, filepath.Join(t.TempDir(), in.Directory), now())
	fixturekit.Must(t, err)
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: in.Turn}
}

// ClaimContestManagerTurn claims the manager event turn that rules on a
// contested unit.
func ClaimContestManagerTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID) coreadapter.Scope {
	t.Helper()
	return ClaimManagerTurn(t, repository, project, stream, func() time.Time { return fixturekit.DemoStart }, ManagerTurnInput{
		Turn: "events_1", Prompt: "Unit resume is contested.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
}

// ManagerEventScope is the scope of the manager event turn that
// ClaimContestManagerTurn claims.
func ManagerEventScope(project config.ProjectID, stream config.WorkstreamID) coreadapter.Scope {
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: "events_1"}
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

// ClaimHeldDriftTurn claims the manager's turn that hands back a drift rebase held for the owner.
func ClaimHeldDriftTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, now func() time.Time, id string) coreadapter.Scope {
	t.Helper()
	return ClaimManagerTurn(t, repository, project, stream, now, ManagerTurnInput{
		Turn: id, Prompt: "The drift rebase is held.", Actor: fixturekit.ServiceActor, Cause: "fixture", Token: "token-" + id, Directory: id,
	})
}
